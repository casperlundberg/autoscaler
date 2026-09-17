BINARY := bin/autoscaler
PKG    := ./...

# What a build is stamped with, so the binary can say which code it is. See
# internal/buildinfo, and scripts/version.sh for how a version is derived.
MODULE   := github.com/casperlundberg/autoscaler
VERSION  ?= $(shell scripts/version.sh 2>/dev/null)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null)
MODIFIED ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo false || echo true)
LDFLAGS  := -X $(MODULE)/internal/buildinfo.version=$(VERSION) \
            -X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
            -X $(MODULE)/internal/buildinfo.modified=$(MODIFIED)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test: ## Run the unit test suite
	go test $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector
	go test -race $(PKG)

.PHONY: cover
cover: ## Run tests and report total coverage
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -l -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: build
build: ## Build the service binary, stamped with its version and commit
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/autoscaler

.PHONY: run
run: build ## Run the service locally
	./$(BINARY)

.PHONY: version
version: ## Print this checkout's semantic version
	@scripts/version.sh

.PHONY: scripts-test
scripts-test: ## Test the version and release scripts against real repositories
	scripts/version_test.sh
	scripts/release_test.sh

.PHONY: release
release: ## Tag and push a release: make release VERSION=1.3.0
	scripts/release.sh $(VERSION)

.PHONY: check
check: fmt-check vet test scripts-test ## Everything CI runs

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out
