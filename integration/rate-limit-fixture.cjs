// SPDX-License-Identifier: MPL-2.0

// Disposable-stack quota setup only, never an HTTP response fixture.
// Audited at Outline 4a5a616a21be800257dc11cef4263d0dd0412156:
// server/middlewares/rateLimiter.ts consumes `${fullPath}:${identifier}`;
// server/utils/RateLimiter.ts derives the credential identifier and Redis prefix.
// The released rate-limiter-flexible 2.4.2 library's set() writes consumed points
// with an expiry, and consume() increments that same bucket before rejecting.
// Use those released helpers instead of guessing Redis keys or changing quotas.
const { version } = require("./package.json");
const limiterVersion = require("rate-limiter-flexible/package.json").version;
const env = require("./build/server/env").default;
const { sequelize } = require("./build/server/storage/database");
const Redis = require("./build/server/storage/redis").default;
const { Team, User, ApiKey, Group, Collection } = require("./build/server/models");
const { default: RateLimiter, RateLimiterStrategy } = require("./build/server/utils/RateLimiter");

const routes = {
  "groups.create": "TenPerMinute",
  "users.invite": "FiftyPerHour",
  "collections.add_user": "OneHundredPerHour",
  "users.delete": "TenPerHour",
};

async function fixture() {
  if (version !== "1.10.1" || limiterVersion !== "2.4.2" || !env.RATE_LIMITER_ENABLED ||
      env.RATE_LIMITER_MULTIPLIER !== 1 || env.RATE_LIMITER_REQUESTS !== 1000 ||
      env.RATE_LIMITER_DURATION_WINDOW !== 60 ||
      env.DATABASE_URL !== "postgres://outline:acceptance-test-password@postgres:5432/outline" ||
      env.REDIS_URL !== "redis://redis:6379" ||
      !/^http:\/\/127\.0\.0\.1:\d+$/.test(env.URL) ||
      process.env.SECRET_KEY !== "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
    throw new Error("Rate-limit fixture requires the pinned disposable Compose stack and default quotas");
  }
  await sequelize.authenticate();
  const team = await Team.findOne({ where: { name: "Terraform acceptance" } });
  const admin = team && await User.findOne({ where: {
    teamId: team.id, email: "terraform-admin@example.invalid", role: "admin"
  } });
  const key = admin && await ApiKey.findOne({ where: {
    userId: admin.id, name: "Terraform acceptance", scope: null
  } });
  if (!team || !admin || !key || !/^[a-f0-9]{64}$/.test(key.hash) || (await Team.count()) !== 1 ||
      (await User.count()) !== 1 || (await ApiKey.count()) !== 1) {
    throw new Error("Refusing quota setup outside the bootstrap-only acceptance workspace");
  }
  const quotas = Object.entries(routes).map(([operation, strategy]) => ({
    operation,
    points: Math.max(1, Math.round(RateLimiterStrategy[strategy].requests * env.RATE_LIMITER_MULTIPLIER)),
    seconds: RateLimiterStrategy[strategy].duration,
  }));
  const action = process.argv[2];
  if (action !== "inspect") {
    const operation = process.argv[3];
    const quota = quotas.find((entry) => entry.operation === operation);
    if (!quota || !["exhaust", "clear"].includes(action)) {
      throw new Error("Expected inspect, exhaust, or clear for an audited route");
    }
    const path = RateLimiter.normalizePath(`/api/${operation}`);
    RateLimiter.setRateLimiter(path, {
      points: quota.points, duration: quota.seconds,
      keyPrefix: RateLimiter.RATE_LIMITER_REDIS_KEY_PREFIX,
      storeClient: Redis.defaultClient,
    });
    const limiter = RateLimiter.getRateLimiter(path);
    // ApiKey.value is creation-only. ApiKey.generateSecret and crypto.hash
    // store the same SHA-256 used by RateLimiter.hashToken, so no plaintext
    // key needs to be passed to this script or saved in another fixture file.
    const identifier = `${RateLimiter.CREDENTIAL_IDENTIFIER_PREFIX}${key.hash}`;
    const bucket = `${path}:${identifier}`;
    if (action === "exhaust") {
      await limiter.set(bucket, quota.points, quota.seconds);
      const actual = await limiter.get(bucket);
      if (!actual || actual.remainingPoints !== 0 || actual.msBeforeNext < (quota.seconds - 5) * 1000) {
        throw new Error("Quota setup did not exhaust the released Redis bucket");
      }
    } else {
      await limiter.delete(bucket);
    }
  }
  const result = {
    version, enabled: env.RATE_LIMITER_ENABLED, multiplier: env.RATE_LIMITER_MULTIPLIER,
    quotas, groups: await Group.count(), users: await User.count(), collections: await Collection.count(),
  };
  await sequelize.close();
  // No key, credential hash, bucket identifier, request, or Redis URL is emitted.
  process.stdout.write(`OUTLINE_ACCEPTANCE_RATE_LIMIT=${JSON.stringify(result)}\n`);
  process.exit(0);
}

fixture().catch(() => {
  // Errors from ORM or Redis could include configuration or credentials.
  console.error("Guarded rate-limit fixture failed");
  process.exit(1);
});
