// SPDX-License-Identifier: MPL-2.0

// Test fixture only. The runner copies this into a disposable released image.
// Outline requires an app session, not an API key, to call apiKeys.create.
// Use the image's ORM and its normal validation/secret-generation hooks instead.
const { version } = require("./package.json");
if (version !== "1.10.1") {
  console.warn(
    `Bootstrap fixture is verified only on Outline 1.10.1, attempting ${version}; compatibility is not claimed`
  );
}

const { sequelize } = require("./build/server/storage/database");
const { Team, User, ApiKey, AuthenticationProvider } = require("./build/server/models");

async function bootstrap() {
  await sequelize.authenticate();
  // Never seed an existing workspace, even if this script runs accidentally twice.
  if (await Team.count()) {
    throw new Error("Refusing to bootstrap a nonempty Outline database");
  }
  const fixture = await sequelize.transaction(async (transaction) => {
    const team = await Team.create(
      { name: "Terraform acceptance" },
      { transaction }
    );
    // Workspace authentication setup only. Target users are invited by Terraform
    // and linked only by the released /auth/oidc.callback route.
    if (process.env.OIDC_CLIENT_ID === "outline-acceptance") {
      if (version !== "1.10.1") {
        throw new Error("OIDC fixture requires Outline 1.10.1");
      }
      await AuthenticationProvider.create(
        { name: "oidc", providerId: "127.0.0.1", teamId: team.id, enabled: true },
        { transaction }
      );
    }
    const user = await User.create(
      {
        name: "Terraform acceptance admin",
        email: "terraform-admin@example.invalid",
        role: "admin",
        teamId: team.id,
        lastSignedInAt: new Date(),
      },
      { transaction }
    );
    const key = await ApiKey.create(
      { name: "Terraform acceptance", userId: user.id, scope: null },
      { transaction }
    );
    if (!key.value || key.scope !== null) {
      throw new Error("ORM did not create an unrestricted API key");
    }
    return { api_key: key.value };
  });
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_FIXTURE=${JSON.stringify(fixture)}\n`);
  // Importing Outline models also starts Redis clients. This one-shot fixture
  // must exit after the transaction and database connection close.
  process.exit(0);
}

bootstrap().catch((error) => {
  console.error(error);
  process.exit(1);
});
