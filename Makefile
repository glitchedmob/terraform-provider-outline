default: fmt test build

build:
	go build -v ./...

generate:
	go tool oapi-codegen --config openapi/oapi-codegen.yaml openapi/outline.openapi.json

check-generated: generate
	git diff --exit-code -- internal/client/
	test -z "$$(git ls-files --others --exclude-standard -- internal/client/)"

generate-docs:
	go tool tfplugindocs generate --provider-name outline --rendered-provider-name Outline

validate-docs:
	go tool tfplugindocs validate --provider-name outline

fmt:
	gofmt -s -w .
	terraform fmt -recursive examples/

fmt-check:
	test -z "$$(gofmt -s -l .)"
	terraform fmt -check -recursive examples/

lint:
	go tool golangci-lint run ./...

test:
	go test -v -cover ./...

# Docker and Terraform are required. Override OUTLINE_VERSION to investigate another release.
# The 25 serial container tests include shared-stack collection grant and IAM cases.
# The 30m bound timed out in CI during the final user tests after all new IAM cases passed.
# Allow 35m for the full suite, cold starts, and five-requests-per-second pacing.
testacc:
	DOCKER_HOST="$${DOCKER_HOST:-$$(docker context inspect --format '{{.Endpoints.docker.Host}}')}" TF_ACC=1 go test -count=1 -v -timeout 35m -artifacts -outputdir="$(CURDIR)" ./internal/provider -run '^TestAcc'

.PHONY: default build generate check-generated generate-docs validate-docs fmt fmt-check lint test testacc
