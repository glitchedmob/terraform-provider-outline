// SPDX-License-Identifier: MPL-2.0

// Only archive metadata and synchronization setup use the ORM. Collection
// grants, group memberships, names, and deletion always use the released API.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, Group, Collection, Document, ExternalGroup, AuthenticationProvider } = require("./build/server/models");

async function fixture() {
  const [action, id] = process.argv.slice(2);
  if (version !== "1.10.1" || !["sync", "unsync", "archive", "restore"].includes(action) ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id || "") ||
      id === "00000000-0000-0000-0000-000000000000") {
    throw new Error("Collection group fixture requires Outline 1.10.1, a known action, and a canonical nonzero UUID");
  }
  await sequelize.authenticate();
  if ((await Team.count({ paranoid: false })) !== 1) {
    throw new Error("Collection group fixture requires one disposable acceptance team");
  }
  const team = await Team.findOne({ where: { name: "Terraform acceptance" } });
  const admin = team && await User.findOne({ where: {
    teamId: team.id, email: "terraform-admin@example.invalid", role: "admin", suspendedAt: null
  } });
  if (!team || !admin) {
    throw new Error("Refusing to change a database outside the disposable acceptance workspace");
  }
  const result = await sequelize.transaction(async (transaction) => {
    if (["archive", "restore"].includes(action)) {
      const collection = await Collection.findByPk(id, {
        paranoid: false, transaction, lock: transaction.LOCK.UPDATE
      });
      if (!collection || collection.teamId !== team.id || collection.createdById !== admin.id ||
          collection.name !== "Terraform collection group archived" || collection.deletedAt ||
          collection.permission !== null || !!collection.archivedAt !== (action === "restore") ||
          (action === "restore" && collection.archivedById !== admin.id) ||
          (await Document.unscoped().count({ where: { collectionId: id }, paranoid: false, transaction })) !== 0) {
        throw new Error("Refusing to archive or restore outside the empty collection group fixture");
      }
      // The test needs archive metadata, not document lifecycle or queue events.
      collection.archivedAt = action === "archive" ? new Date() : null;
      collection.archivedById = action === "archive" ? admin.id : null;
      await collection.save({ fields: ["archivedAt", "archivedById"], hooks: false, transaction });
      return { collection_id: collection.id, archived: !!collection.archivedAt };
    }
    const group = await Group.findByPk(id, { transaction, lock: transaction.LOCK.UPDATE });
    if (!group || group.teamId !== team.id ||
        group.name !== "Terraform collection group synchronized" || group.externalId) {
      throw new Error("Refusing to change synchronization outside the collection group fixture");
    }
    const marker = `terraform-collection-group-${group.id}`;
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
    const synced = (await ExternalGroup.count({ where: { groupId: group.id }, transaction })) !== 0;
    if (synced !== (action === "sync")) {
      throw new Error("Synchronization fixture did not confirm the requested change");
    }
    return { group_id: group.id, synced };
  });
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_COLLECTION_GROUP=${JSON.stringify({ version, action, ...result })}\n`);
  // Importing models starts Redis clients too. This process runs only once.
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
