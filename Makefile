GO      ?= go
BINARY  := bin/forge
PKGS    := ./...

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the forge binary into ./bin/forge
	$(GO) build -o $(BINARY) ./cmd/forge

.PHONY: test
test: ## Run unit tests (no root required)
	$(GO) test -race $(PKGS)

.PHONY: test-integration
test-integration: ## Run privileged integration tests (requires root, Linux)
	$(GO) test -tags integration -count=1 ./test/integration/...

.PHONY: cover
cover: ## Report unit-test coverage
	$(GO) test -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format all Go source
	$(GO) fmt $(PKGS)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin coverage.out

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
