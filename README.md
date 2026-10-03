# Terraform provider for Outline

[![Tests](https://github.com/glitchedmob/terraform-provider-outline/actions/workflows/test.yml/badge.svg)](https://github.com/glitchedmob/terraform-provider-outline/actions/workflows/test.yml)

The Outline provider manages workspace users, manually maintained groups, group memberships, and active collections through Terraform. It looks up users by UUID or exact normalized email, and groups and collections by UUID or exact name. The API contracts and acceptance stack target Outline 1.10.1 only. Compatibility with other releases is not claimed.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) 1.0 or later
- [Go](https://go.dev/doc/install) 1.27.1 or later

The provider uses Terraform Plugin Framework v1.19.0 and protocol 6.

## Authentication

Set `OUTLINE_API_KEY` to an Outline API key. For a self-hosted instance, set `OUTLINE_BASE_URL` to its API URL, including `/api`. The default is `https://app.getoutline.com/api`.

Explicit `api_key` and `base_url` attributes override their environment variables. The key is sensitive and sent as `Authorization: Bearer <api_key>`. Configuration validates local values and creates an HTTP client without making API requests. It does not verify the key or contact the server.

Use an unrestricted admin-owned key for group management and lookup. User, group membership, and collection operations require an active workspace admin. User modifying actions refuse to target the API-key owner; use another admin's key to manage that account. Group membership operations support the key owner's membership and do not change workspace roles. The transport paces requests at five per second per configured client and honors cancellation. It does not replay writes on errors or rate limits; 429 errors report `Retry-After`.

See [provider configuration](docs/index.md) for the schema, timeout, and example.

## Resources and data sources

- [User resource](docs/resources/user.md), manages accounts by UUID, with explicit suspension state and suspension on destroy by default
- [User data source](docs/data-sources/user.md), looks up one account by UUID or exact normalized email, including pending and suspended accounts
- [Group resource](docs/resources/group.md), manages manual groups by UUID and rejects externally synchronized groups
- [Group data source](docs/data-sources/group.md), looks up one group by UUID or exact, case-sensitive name
- [Group member resource](docs/resources/group_member.md), manages one user's membership and permission in a manual group, with `group_id/user_id` import
- [Collection resource](docs/resources/collection.md), manages active collections by stable UUID, with private default access, public sharing disabled, and destruction blocked by default
- [Collection data source](docs/data-sources/collection.md), reads active or archived collection metadata by UUID or exact, case-sensitive name, including private collections

Collection default access is separate from direct user and group grants, which this provider does not manage. Outline grants the creator direct collection-admin membership. Omitted collection `permission` means private, not unmanaged; import existing collections by stable UUID and match `permission`, `description`, and `sharing` before applying. Soft deletion trashes published collection content and has no provider restore operation. Apply `allow_destroy = true` before destruction, or remove state and configuration to stop managing without deletion. Admin metadata access does not grant access to private documents.

See the [import guide](docs/guides/import.md) for collections, users, groups, and group memberships. See [provisioning users for OIDC](docs/guides/oidc.md) for invitation, import, offboarding, and recovery behavior. The provider does not configure authentication or perform an OIDC handshake.

## Development

```shell
git clone git@github.com:glitchedmob/terraform-provider-outline.git
cd terraform-provider-outline
make fmt-check
make lint
make test
make build
```

`make lint` runs `go tool golangci-lint` v2.13.2, pinned in the root `go.mod`. No separate installer is needed. `make test` covers provider configuration, group, user, group membership, and collection behavior, schema validation, protocol 6, generated IAM contracts, timeouts, bearer headers, and redirect protection using local HTTP fixtures. With `TF_ACC` unset, acceptance tests skip. Unit tests need no Outline credentials, Docker, or running instance. Local HTTP tests drive Terraform itself to verify failed-create taint and `terraform untaint` recovery for groups, users, memberships, and collections. If `terraform` on PATH is a version-manager shim that cannot run with an isolated HOME, set `TF_ACC_TERRAFORM_PATH` to the installed Terraform executable for both `make test` and `make testacc`.

The official Outline OpenAPI source is pinned and committed under `openapi/`, separate from a release-verified correction overlay. `make generate` uses oapi-codegen v2.8.0 to generate Go models and client methods for 29 IAM operations, including `users.update` with a required target UUID. `make check-generated` rejects tracked and untracked client drift without fetching a live spec. See [API generation](openapi/README.md) for provenance, v1.10.1 compatibility, corrections, calling conventions, and limits. Commit generated `internal/client/client.gen.go` with its sources.

### Acceptance tests

Install Terraform and Docker, start a local Docker daemon, and select its context. Then run:

```shell
make testacc
```

`make testacc` sets `TF_ACC=1` and runs `TestAcc` tests with a 20-minute timeout. The 23 serial container tests took about 12 minutes locally; the extra time covers cold starts and slower CI hosts. The acceptance job allows 30 minutes for setup, image pulls, compilation, tests, and cleanup. It preserves an explicit `DOCKER_HOST`; otherwise it uses the active Docker context's endpoint. Testcontainers starts a separate Compose project for each test from `integration/compose.yml`, with Outline 1.10.1, PostgreSQL 16, and Redis 7. Outline binds to a dynamically chosen loopback port. The first run needs access to the container registries to pull the images.

The runner clears inherited `OUTLINE_API_KEY` and `OUTLINE_BASE_URL`. It never tests against an instance or credentials supplied through those variables. After the services are healthy, it copies `integration/bootstrap.cjs` into the disposable Outline container. The fixture refuses a nonempty database, warns when the image is not version 1.10.1, then uses the released image's ORM hooks to create a team, admin, and unrestricted API key. Tests pass that ephemeral key and local API URL explicitly in their provider configuration. The fixed database password and application secrets in Compose are test-only values, not deployment credentials.

Group tests cover the lifecycle, import, drift correction, out-of-band deletion, and ID/name lookups across multiple pages, including duplicate and missing names. The release rejects duplicate manual names, so `integration/duplicate-group.cjs` inserts one duplicate into the isolated database with ORM validation and hooks disabled. Terraform still looks it up through real HTTP. The test then renames it and deletes it through the API. Compose disables the server's rate limiter to populate pagination fixtures; provider pacing and 429 behavior have separate HTTP unit tests. Cleanup captures service logs and removes the project's containers and volumes, including after a failed startup. Go saves logs under `_artifacts/`; the acceptance workflow uploads them on failure. Treat test artifacts as sensitive because test configuration contains the ephemeral API key.

User acceptance tests exercise admin API provisioning without SMTP. The API roles `admin`, `member`, `viewer`, and `guest` are tested against Outline 1.10.1; editions, licenses, key scopes, and server policies may still reject role changes. No fake IdP is included. These tests do not verify an OIDC handshake, token exchange, first SSO sign-in, or identity linking.

Group member acceptance tests cover pending invited users, `member`/`admin` permission changes, import, existing-pair refusal, drift correction, removal, and refresh after out-of-band membership or parent deletion. They check that membership changes preserve workspace roles and unrelated memberships. The fixtures need no passwords, SMTP, or sign-in. The live pagination fixture has 102 memberships with the target after page one. Tests also cover self-membership, forbidden responses, and refusal after a group becomes synchronized. `integration/group-member-fixture.cjs` uses guarded ORM setup only to attach or detach synchronization in the disposable workspace. Malformed-response handling has local HTTP tests. The server's same-pair concurrent upsert race cannot be prevented by preflight; coordinate writers as described in the [resource documentation](docs/resources/group_member.md).

Collection acceptance tests cover CRUD, stable UUIDs, Markdown, private/read/read_write transitions, sharing, import, drift, remote deletion, and paginated exact-name lookups with duplicate names. They verify admin-only list visibility, archived lookups, unsupported resource defaults, cross-workspace 403 state retention, and the release's omitted-permission grant side effect. Deletion tests observe trashed published documents, surviving archives, detached drafts, and retained grant rows. `integration/collection-fixture.cjs` only seeds or observes those disposable documents, toggles empty test archives, or creates a second isolated workspace. No document or collection-grant resources are added.

The CI matrix starts with `1.10.1`. To investigate a future release, run `OUTLINE_VERSION=<release> make testacc`. The override selects only the disposable image, never a live instance. The ORM fixture may need changes for another release; passing an override is not a compatibility claim. Only 1.10.1 has been tested.

### Documentation

`docs/` is generated by HashiCorp tfplugindocs v0.25.0, pinned as a tool in the root `go.mod`. Do not edit generated pages directly. The OIDC and import guides are static sources at `templates/guides/oidc.md` and `templates/guides/import.md`, copied into `docs/guides/` during generation.

- Edit page prose and warnings in `templates/`.
- Edit schema descriptions in Go `MarkdownDescription` strings under `internal/provider/`. Include defaults and validator constraints there; Terraform's schema export does not expose them.
- Edit Terraform examples in `examples/`. Resource `import.sh` files are documentation snippets. Replace placeholder UUIDs and match the target configuration before importing. Group member import uses `group_id/user_id`; configure `permission = "admin"` for an imported admin membership or the default will reconcile it to `member`. Collection import uses a stable UUID, not a short URL ID; match current access, description, and sharing settings to avoid applying defaults after import.

Run `make generate-docs` and `make validate-docs`, then `make fmt-check`. Commit the sources and generated `docs/` together. Generation needs Go and Terraform, but no Outline credentials or Docker. Docs CI uses Terraform 1.14.7, checks tracked and untracked output for drift, and validates Registry structure. Validation does not execute examples or imports.

### Releases

The tag-triggered release workflow follows the Kaneo repository's signed GoReleaser configuration, with GoReleaser pinned to v2.18.2. It needs repository secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE` before a release can succeed. This repository does not configure those secrets.

The provider has not been published to the Terraform Registry. The source address in examples is the intended address and is not installable from the Registry yet. Building the provider or generating documentation does not publish a release.

## License

Provider code is licensed under the [Mozilla Public License 2.0](LICENSE). The upstream OpenAPI specification and code generated from it retain the [BSD-3-Clause license](openapi/LICENSE).
