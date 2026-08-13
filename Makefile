# certforge-issuer — developer targets
.PHONY: build docker-build test test-integration lint help

BINARY      ?= certforge-issuer
IMAGE       ?= certforge-issuer:dev
GO_FLAGS    ?=

## build: compile the controller binary
build:
	go build $(GO_FLAGS) -o bin/$(BINARY) ./cmd/controller

## docker-build: build the Docker image
docker-build:
	docker build -t $(IMAGE) .

## test: run unit tests
test:
	go test ./...

## lint: run go vet
lint:
	go vet ./...

## test-integration: run the full cert-manager integration test suite
##
##   Requires:  kind, kubectl, helm, docker, openssl, curl, jq, envsubst
##   Config:    copy test/integration/.env.example to test/integration/.env and fill in values
##
##   Quick run (reuse existing cluster + image):
##     KEEP_CLUSTER=1 SKIP_BUILD=1 make test-integration
##
test-integration:
	@if [ -f test/integration/.env ]; then \
		set -a; . test/integration/.env; set +a; \
	fi; \
	exec test/integration/run.sh

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## //'
