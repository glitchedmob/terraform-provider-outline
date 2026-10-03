// SPDX-License-Identifier: MPL-2.0

// Test-only inspection and API-key creation in the disposable acceptance team.
// User creation, role/name changes, suspension, and deletion use the HTTP API.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, ApiKey } = require("./build/server/models");
const { UserFlag } = require("./build/server/models/User");
const env = require("./build/server/env").default;

async function fixture() {
  await sequelize.authenticate();
  if ((await Team.count()) !== 1) {
    throw new Error("User fixture requires one disposable acceptance team");
  }
  const user = await User.findByPk(process.argv[2]);
  const team = user && (await Team.findByPk(user.teamId));
  const admin = team && (await User.findOne({ where: {
    teamId: team.id, email: "terraform-admin@example.invalid", role: "admin"
  } }));
  if (!user || !team || team.name !== "Terraform acceptance" || !admin) {
    throw new Error("Refusing to inspect or create keys outside the acceptance workspace");
  }
  const result = {
    version,
    user_id: user.id,
    pending: user.isInvited,
    last_signed_in_at: user.lastSignedInAt,
    invitation_email_sent: !!user.getFlag(UserFlag.InviteSent),
    password_attribute: Object.keys(User.getAttributes()).some((name) => /password/i.test(name)),
    email_enabled: !!env.EMAIL_ENABLED,
    suspended_at: user.suspendedAt,
    suspended_by_id: user.suspendedById,
  };
  if (process.argv[3] === "key") {
    const key = await ApiKey.create({ name: "Terraform user acceptance", userId: user.id, scope: null });
    if (!key.value || key.scope !== null) {
      throw new Error("ORM did not create an unrestricted API key");
    }
    result.api_key = key.value;
  } else if (process.argv[3] !== "inspect") {
    throw new Error("Expected inspect or key action");
  }
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_USER=${JSON.stringify(result)}\n`);
  // Model imports also start Redis clients. This fixture runs once per process.
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
