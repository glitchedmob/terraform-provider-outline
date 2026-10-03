// SPDX-License-Identifier: MPL-2.0

// Disposable, manually configured OIDC provider. No passwords, external calls,
// request logging, or production credentials. Never deploy this service.
const http = require("node:http");
const crypto = require("node:crypto");
const callback = process.env.OUTLINE_CALLBACK_URL;
if (!/^http:\/\/127\.0\.0\.1:\d+\/auth\/oidc\.callback$/.test(callback || "")) {
  throw new Error("Expected a loopback Outline callback");
}
const clientId = "outline-acceptance";
const clientSecret = "oidc-acceptance-only-secret";
const cases = new Set(["verified", "id-token", "string-true", "missing", "false", "userinfo-false"]);
const codes = new Map();
const tokens = new Map();
const stats = Object.fromEntries([...cases].map(name => [name, { authorize: 0, token: 0, userinfo: 0 }]));
const { privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });

function claims(name, idToken) {
  const profile = {
    sub: `fixture-${name}`,
    email: `${name}@example.invalid`,
    preferred_username: `OIDC ${name}`,
  };
  if (name === "verified" || name === "id-token" && idToken) profile.email_verified = true;
  if (name === "string-true") profile.email_verified = "true";
  if (name === "false") profile.email_verified = false;
  if (name === "userinfo-false") profile.email_verified = idToken;
  // Exercise the router's email/name/sub fallback, not just its claim fallback.
  if (name === "id-token" && !idToken) return { sub: profile.sub };
  return profile;
}

function jwt(payload) {
  const encode = value => Buffer.from(JSON.stringify(value)).toString("base64url");
  const body = `${encode({ alg: "RS256", typ: "JWT" })}.${encode(payload)}`;
  return `${body}.${crypto.sign("RSA-SHA256", Buffer.from(body), privateKey).toString("base64url")}`;
}

function json(res, status, body) {
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(JSON.stringify(body));
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, "http://oidc:8080");
    if (req.method === "GET" && url.pathname === "/health") return json(res, 200, { ok: true });
    if (req.method === "GET" && url.pathname === "/stats") return json(res, 200, stats);
    if (req.method === "GET" && url.pathname === "/authorize") {
      const q = url.searchParams;
      const name = q.get("fixture_case");
      if (!cases.has(name) || q.get("client_id") !== clientId || q.get("response_type") !== "code" ||
          q.get("redirect_uri") !== callback || !q.get("state") || !q.get("scope")?.split(" ").includes("openid")) {
        return json(res, 400, { error: "invalid_request" });
      }
      const code = crypto.randomBytes(32).toString("hex");
      codes.set(code, { name, expires: Date.now() + 60000, nonce: q.get("nonce") });
      stats[name].authorize++;
      const redirect = new URL(callback);
      redirect.searchParams.set("code", code);
      redirect.searchParams.set("state", q.get("state"));
      res.writeHead(302, { Location: redirect.toString(), "Cache-Control": "no-store" });
      return res.end();
    }
    if (req.method === "POST" && url.pathname === "/token") {
      let body = "";
      for await (const chunk of req) {
        body += chunk;
        if (body.length > 8192) return json(res, 400, { error: "invalid_request" });
      }
      const q = new URLSearchParams(body);
      const basic = "Basic " + Buffer.from(`${clientId}:${clientSecret}`).toString("base64");
      if (req.headers.authorization !== basic && (q.get("client_id") !== clientId || q.get("client_secret") !== clientSecret)) {
        return json(res, 401, { error: "invalid_client" });
      }
      const record = codes.get(q.get("code"));
      codes.delete(q.get("code"));
      if (!record || record.expires < Date.now() || q.get("grant_type") !== "authorization_code" || q.get("redirect_uri") !== callback) {
        return json(res, 400, { error: "invalid_grant" });
      }
      const accessToken = crypto.randomBytes(32).toString("hex");
      tokens.set(accessToken, record);
      stats[record.name].token++;
      const now = Math.floor(Date.now() / 1000);
      return json(res, 200, {
        access_token: accessToken, token_type: "Bearer", expires_in: 60, scope: "openid profile email",
        id_token: jwt({ ...claims(record.name, true), iss: "http://oidc:8080", aud: clientId, iat: now, exp: now + 60,
          ...(record.nonce ? { nonce: record.nonce } : {}) }),
      });
    }
    if (req.method === "GET" && url.pathname === "/userinfo") {
      const record = tokens.get((req.headers.authorization || "").replace(/^Bearer /, ""));
      if (!record || record.expires < Date.now()) return json(res, 401, { error: "invalid_token" });
      stats[record.name].userinfo++;
      return json(res, 200, claims(record.name, false));
    }
    return json(res, 404, { error: "not_found" });
  } catch {
    // Do not print request URLs, codes, tokens, claims, or client secrets.
    return json(res, 500, { error: "fixture_error" });
  }
});
server.listen(8080, "0.0.0.0");
process.on("SIGTERM", () => server.close());
