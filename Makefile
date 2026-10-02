default: fmt test build

build:
	go build -v ./...

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

.PHONY: default build generate-docs validate-docs fmt fmt-check lint test
