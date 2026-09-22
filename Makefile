# AGROGRAM backend: contract tooling and local infrastructure.
#
# proto-* targets run buf from the official bufbuild/buf Docker image, so the
# only host requirement is Docker (plus network access to buf.build for deps and
# remote plugins). No local protoc, buf or protoc-gen-* plugins.
# infra-* targets run the local stack in deploy/ with Docker Compose (see
# deploy/README.md).

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

# Local infrastructure. Settings and secrets come from INFRA_ENV_FILE; create
# it once with `cp deploy/.env.example deploy/.env`. SERVICE limits infra-logs
# to one service, e.g. `make infra-logs SERVICE=kafka`.
INFRA_ENV_FILE ?= deploy/.env
SERVICE        ?=
INFRA_COMPOSE  := $(DOCKER) compose --file deploy/docker-compose.yml --env-file "$(INFRA_ENV_FILE)"

.DEFAULT_GOAL := help

.PHONY: help proto proto-deps proto-build proto-lint proto-format proto-format-check \
	proto-breaking proto-gen proto-clean \
	infra-env-check infra-up infra-down infra-ps infra-logs

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

# Fails fast with a hint, instead of Compose's "required variable" errors.
infra-env-check:
	@test -f "$(INFRA_ENV_FILE)" || { \
		echo "$(INFRA_ENV_FILE) not found. Create it from the template, then change the passwords:" >&2; \
		echo "  cp deploy/.env.example deploy/.env" >&2; \
		exit 1; }

infra-up: infra-env-check ## Start Postgres, Redis, Kafka and the UIs; wait until healthy
	$(INFRA_COMPOSE) up --detach --wait --wait-timeout 300 || { $(INFRA_COMPOSE) ps --all; exit 1; }
	$(INFRA_COMPOSE) ps --all

infra-down: infra-env-check ## Stop and remove the infra containers (data volumes are kept)
	$(INFRA_COMPOSE) down --remove-orphans

infra-ps: infra-env-check ## Show infra containers and health (kafka-init: Exited (0))
	$(INFRA_COMPOSE) ps --all

infra-logs: infra-env-check ## Follow infra logs; one service only: SERVICE=kafka
	$(INFRA_COMPOSE) logs --follow --tail 200 $(SERVICE)
