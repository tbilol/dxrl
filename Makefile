# AGROGRAM backend: contract tooling.
#
# Every target runs buf from the official bufbuild/buf Docker image, so the only
# host requirement is Docker (plus network access to buf.build for deps and
# remote plugins). No local protoc, buf or protoc-gen-* plugins.

BUF_VERSION ?= 1.73.0
BUF_IMAGE   ?= bufbuild/buf:$(BUF_VERSION)
DOCKER      ?= docker

# Git ref the breaking-change check compares against. Needs the contracts to be
# committed on that branch first.
BREAKING_AGAINST ?= .git\#branch=main

# Run as the host user so generated files are not root-owned; keep buf's module
# and plugin cache inside the repo (git-ignored) so it survives between runs.
# Export BUF_TOKEN to authenticate against the BSR (higher rate limits).
BUF := $(DOCKER) run --rm \
	--volume "$(CURDIR):/workspace" \
	--workdir /workspace \
	--user "$$(id -u):$$(id -g)" \
	--env BUF_CACHE_DIR=/workspace/.buf-cache \
	--env BUF_TOKEN \
	$(BUF_IMAGE)

.DEFAULT_GOAL := help

.PHONY: help proto proto-deps proto-build proto-lint proto-format proto-format-check \
	proto-breaking proto-gen proto-clean

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-20s %s\n", $$1, $$2}'

proto: proto-format-check proto-lint proto-gen ## Format check, lint, then generate code

proto-deps: ## Resolve deps and update buf.lock
	$(BUF) dep update

proto-build: ## Compile all contracts (fast syntax/type check)
	$(BUF) build

proto-lint: ## Lint contracts (style + protovalidate rules)
	$(BUF) lint

proto-format: ## Rewrite .proto files in canonical format
	$(BUF) format --write

proto-format-check: ## Fail if any .proto file is not canonically formatted
	$(BUF) format --diff --exit-code

proto-breaking: ## Detect breaking changes against BREAKING_AGAINST
	$(BUF) breaking --against '$(BREAKING_AGAINST)'

proto-gen: ## Generate Go, Python and Java code into gen/
	$(BUF) generate

proto-clean: ## Remove generated code
	rm -rf gen
