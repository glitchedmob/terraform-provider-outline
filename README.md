# Terraform provider for Outline

[![Tests](https://github.com/glitchedmob/terraform-provider-outline/actions/workflows/test.yml/badge.svg)](https://github.com/glitchedmob/terraform-provider-outline/actions/workflows/test.yml)

The Outline provider manages manually maintained groups through Terraform and looks up existing groups by UUID or exact name. The API contracts and acceptance stack target Outline 1.10.1 only. Compatibility with other releases is not claimed.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) 1.0 or later
- [Go](https://go.dev/doc/install) 1.27.1 or later

The provider uses Terraform Plugin Framework v1.19.0 and protocol 6.

## Authentication

Set `OUTLINE_API_KEY` to an Outline API key. For a self-hosted instance, set `OUTLINE_BASE_URL` to its API URL, including `/api`. The default is `https://app.getoutline.com/api`.

Explicit `api_key` and `base_url` attributes override their environment variables. The key is sensitive and sent as `Authorization: Bearer <api_key>`. Configuration validates local values and creates an HTTP client without making API requests. It does not verify the key or contact the server.

Group management and lookup require an unrestricted admin-owned key. The transport paces requests at five per second per configured client and honors cancellation. It does not replay writes on errors or rate limits; 429 errors report `Retry-After`.

See [provider configuration](docs/index.md) for the schema, timeout, and example.

## Resources and data sources

- [Group resource](docs/resources/group.md), manages manual groups by UUID and rejects externally synchronized groups
- [Group data source](docs/data-sources/group.md), looks up one group by UUID or exact, case-sensitive name

## Development

```shell
git clone git@github.com:glitchedmob/terraform-provider-outline.git
cd terraform-provider-outline
make fmt-check
make lint
make test
make build
```

`make lint` runs `go tool golangci-lint` v2.13.2, pinned in the root `go.mod`. No separate installer is needed. `make test` covers provider configuration, group behavior, schema validation, protocol 6, generated IAM contracts, timeouts, bearer headers, and redirect protection using local HTTP fixtures. With `TF_ACC` unset, acceptance tests skip. Unit tests need no Outline credentials, Docker, or running instance. One local HTTP test drives Terraform itself to verify failed-create taint and `terraform untaint` recovery. If `terraform` on PATH is a version-manager shim that cannot run with an isolated HOME, set `TF_ACC_TERRAFORM_PATH` to the installed Terraform executable for both `make test` and `make testacc`.

The official Outline OpenAPI source is pinned and committed under `openapi/`, separate from a release-verified correction overlay. `make generate` uses oapi-codegen v2.8.0 to generate Go models and client methods for 28 IAM operations. `make check-generated` rejects tracked and untracked client drift without fetching a live spec. See [API generation](openapi/README.md) for provenance, v1.10.1 compatibility, corrections, calling conventions, and limits. Commit generated `internal/client/client.gen.go` with its sources.

### Acceptance tests

Install Terraform and Docker, start a local Docker daemon, and select its context. Then run:

```shell
make testacc
```

`make testacc` sets `TF_ACC=1` and runs `TestAcc` tests with a 15-minute timeout. It preserves an explicit `DOCKER_HOST`; otherwise it uses the active Docker context's endpoint. Testcontainers starts a separate Compose project for each test from `integration/compose.yml`, with Outline 1.10.1, PostgreSQL 16, and Redis 7. Outline binds to a dynamically chosen loopback port. The first run needs access to the container registries to pull the images.

The runner clears inherited `OUTLINE_API_KEY` and `OUTLINE_BASE_URL`. It never tests against an instance or credentials supplied through those variables. After the services are healthy, it copies `integration/bootstrap.cjs` into the disposable Outline container. The fixture refuses a nonempty database, warns when the image is not version 1.10.1, then uses the released image's ORM hooks to create a team, admin, and unrestricted API key. Tests pass that ephemeral key and local API URL explicitly in their provider configuration. The fixed database password and application secrets in Compose are test-only values, not deployment credentials.

Tests cover the group lifecycle, import, drift correction, out-of-band deletion, and ID/name lookups across multiple pages, including duplicate and missing names. The release rejects duplicate manual names, so `integration/duplicate-group.cjs` inserts one duplicate into the isolated database with ORM validation and hooks disabled. Terraform still looks it up through real HTTP. The test then renames it and deletes it through the API. Compose disables the server's rate limiter to populate pagination fixtures; provider pacing and 429 behavior have separate HTTP unit tests. Cleanup captures service logs and removes the project's containers and volumes, including after a failed startup. Go saves logs under `_artifacts/`; the acceptance workflow uploads them on failure. Treat test artifacts as sensitive because test configuration contains the ephemeral API key.

The CI matrix starts with `1.10.1`. To investigate a future release, run `OUTLINE_VERSION=<release> make testacc`. The override selects only the disposable image, never a live instance. The ORM fixture may need changes for another release; passing an override is not a compatibility claim. Only 1.10.1 has been tested.

### Documentation

`docs/` is generated by HashiCorp tfplugindocs v0.25.0, pinned as a tool in the root `go.mod`. Do not edit generated pages directly.

- Edit page prose and warnings in `templates/`.
- Edit schema descriptions in Go `MarkdownDescription` strings under `internal/provider/`. Include defaults and validator constraints there; Terraform's schema export does not expose them.
- Edit Terraform examples in `examples/`. Resource `import.sh` files are documentation snippets. Replace placeholder UUIDs and match the target configuration before importing.

Run `make generate-docs` and `make validate-docs`, then `make fmt-check`. Commit the sources and generated `docs/` together. Generation needs Go and Terraform, but no Outline credentials or Docker. Docs CI uses Terraform 1.14.7, checks tracked and untracked output for drift, and validates Registry structure. Validation does not execute examples or imports.

### Releases

The tag-triggered release workflow follows the Kaneo repository's signed GoReleaser configuration, with GoReleaser pinned to v2.18.2. It needs repository secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE` before a release can succeed. This repository does not configure those secrets.

The provider has not been published to the Terraform Registry. The source address in examples is the intended address and is not installable from the Registry yet. Building the provider or generating documentation does not publish a release.

## License

Provider code is licensed under the [Mozilla Public License 2.0](LICENSE). The upstream OpenAPI specification and code generated from it retain the [BSD-3-Clause license](openapi/LICENSE).
