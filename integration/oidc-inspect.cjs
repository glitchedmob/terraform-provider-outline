// SPDX-License-Identifier: MPL-2.0

// Read-only observations after the real auth callback. Never creates a target,
// authentication record, key, or session, and never emits stored tokens.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, UserAuthentication } = require("./build/server/models");

async function inspect() {
  await sequelize.authenticate();
  if (version !== "1.10.1" || (await Team.count()) !== 1 || process.env.OIDC_CLIENT_ID !== "outline-acceptance") {
    throw new Error("Expected the disposable Outline 1.10.1 OIDC workspace");
  }
  const user = await User.findByPk(process.argv[2]);
  const team = user && await Team.findByPk(user.teamId);
  if (!team || team.name !== "Terraform acceptance" || !user.email.endsWith("@example.invalid")) {
    throw new Error("Refusing to inspect outside the acceptance workspace");
  }
  const authentications = await UserAuthentication.findAll({
    where: { userId: user.id }, attributes: ["providerId"],
  });
  const result = {
    pending: user.isInvited,
    last_signed_in: !!user.lastSignedInAt,
    authentications: authentications.map(auth => auth.providerId),
    matching_users: await User.count({ where: { teamId: team.id, email: user.email }, paranoid: false }),
    total_users: await User.count({ where: { teamId: team.id }, paranoid: false }),
  };
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_OIDC=${JSON.stringify(result)}\n`);
  process.exit(0);
}
inspect().catch(() => {
  console.error("OIDC read-only inspection failed");
  process.exit(1);
});
