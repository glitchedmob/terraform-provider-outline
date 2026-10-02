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

.PHONY: default build generate check-generated generate-docs validate-docs fmt fmt-check lint test
