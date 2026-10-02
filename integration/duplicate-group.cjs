// SPDX-License-Identifier: MPL-2.0

// Test fixture only, copied into the disposable acceptance container.
// Outline 1.10.1 rejects duplicate names through groups.create. Bypass only
// ORM validation/hooks to exercise defensive ambiguity handling over real HTTP.
const { sequelize } = require("./build/server/storage/database");
const { Team, User, Group } = require("./build/server/models");

async function createDuplicate() {
  await sequelize.authenticate();
  if ((await Team.count()) !== 1) {
    throw new Error("Duplicate fixture requires one disposable acceptance team");
  }
  const fixture = await sequelize.transaction(async (transaction) => {
    const source = await Group.findByPk(process.argv[2], { transaction });
    if (!source) {
      throw new Error("Duplicate fixture source group does not exist");
    }
    const team = await Team.findByPk(source.teamId, { transaction });
    const user = await User.findByPk(source.createdById, { transaction });
    if (
      !team ||
      team.name !== "Terraform acceptance" ||
      !user ||
      user.role !== "admin" ||
      user.email !== "terraform-admin@example.invalid" ||
      user.teamId !== team.id
    ) {
      throw new Error("Refusing to inject a duplicate outside the acceptance workspace");
    }
    const group = await Group.create(
      {
        name: source.name,
        teamId: team.id,
        createdById: user.id,
        disableMentions: false,
      },
      { transaction, validate: false, hooks: false }
    );
    return { group_id: group.id };
  });
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_DUPLICATE=${JSON.stringify(fixture)}\n`);
  // Model imports start Redis clients too. This is a one-shot fixture.
  process.exit(0);
}

createDuplicate().catch((error) => {
  console.error(error);
  process.exit(1);
});
