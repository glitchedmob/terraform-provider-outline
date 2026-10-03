// SPDX-License-Identifier: MPL-2.0

// Only external synchronization setup needs the ORM. Memberships and users use
// the HTTP API, including the pagination fixture's batched invitations.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, Group, ExternalGroup, AuthenticationProvider } = require("./build/server/models");

async function fixture() {
  const [groupId, action] = process.argv.slice(2);
  if (version !== "1.10.1" || !["sync", "unsync"].includes(action)) {
    throw new Error("Group member fixture requires Outline 1.10.1 and sync or unsync");
  }
  await sequelize.authenticate();
  if ((await Team.count({ paranoid: false })) !== 1) {
    throw new Error("Group member fixture requires one disposable acceptance team");
  }
  const group = await Group.findByPk(groupId);
  const team = group && (await Team.findByPk(group.teamId));
  const admin = team && (await User.findOne({ where: {
    teamId: team.id, email: "terraform-admin@example.invalid", role: "admin", suspendedAt: null
  } }));
  if (!group || !team || team.name !== "Terraform acceptance" || !admin ||
      !group.name.startsWith("Terraform group member ") || group.externalId) {
    throw new Error("Refusing to change external synchronization outside the disposable fixture");
  }
  const marker = `terraform-group-member-${group.id}`;
  await sequelize.transaction(async (transaction) => {
    const links = await ExternalGroup.findAll({ where: { groupId: group.id }, transaction });
    let provider = await AuthenticationProvider.findOne({ where: {
      teamId: team.id, providerId: marker, name: "oidc"
    }, transaction });
    if (links.some((link) => link.teamId !== team.id || link.externalId !== marker ||
        !provider || link.authenticationProviderId !== provider.id)) {
      throw new Error("Refusing to change an external group not owned by this fixture");
    }
    if (action === "sync") {
      if (!provider) {
        provider = await AuthenticationProvider.create({
          name: "oidc", providerId: marker, teamId: team.id, enabled: false, settings: {}
        }, { transaction });
      }
      if (links.length === 0) {
        await ExternalGroup.create({
          externalId: marker, name: group.name, lastSyncedAt: new Date(),
          groupId: group.id, teamId: team.id, authenticationProviderId: provider.id
        }, { transaction });
      }
    } else {
      for (const link of links) {
        await link.destroy({ transaction });
      }
      if (provider) {
        if (await ExternalGroup.count({ where: { authenticationProviderId: provider.id }, transaction })) {
          throw new Error("Refusing to remove an authentication provider with other groups");
        }
        await provider.destroy({ transaction });
      }
    }
  });
  const synced = (await ExternalGroup.count({ where: { groupId: group.id } })) !== 0;
  if (synced !== (action === "sync")) {
    throw new Error("Synchronization fixture did not confirm the requested change");
  }
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_GROUP_MEMBER=${JSON.stringify({ version, group_id: group.id, synced })}\n`);
  // Importing models starts Redis clients too. This process runs only once.
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
