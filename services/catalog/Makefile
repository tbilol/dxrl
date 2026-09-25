# catalog: the farmers' offer board, the resellers' buy-request board, listing
# search and the product reference data.
#
# Run every target from this directory (back/services/catalog).
#
#   gen       delegates to the shared tooling, back/tools/codegen.sh: it needs
#             curl and network access only, downloads the pinned buf release
#             binary into a user cache OUTSIDE the repo, verifies it against the
#             release's sha256.txt, runs `buf generate` from back/ with
#             back/buf.gen.yaml (remote BSR plugins), and writes back/gen/go/
#             go.mod so the generated Go code is a module. `make proto-gen-local`
#             in back/ runs exactly the same thing.
#   Go        targets need a Go toolchain; go.mod selects the version and
#             GOTOOLCHAIN=auto (the default) downloads it.
#   run, migrate-*, test-integration
#             load ./.env when it exists (cp .env.example .env).
#   docker-*, compose-*
#             run Docker; the compose targets layer compose.yaml on top of
#             back/deploy/docker-compose.yml (see README.md).

SHELL := /bin/sh
.DEFAULT_GOAL := help

BACK_DIR := ../..

# Shared codegen tooling: defines CODEGEN (back/tools/codegen.sh), GEN_GO_DIR
# and the single pinned BUF_VERSION. Override with BUF_VERSION=, TOOLS_DIR= (the
# binary cache) or BUF=<path to an existing buf>.
include $(BACK_DIR)/tools/codegen.mk

GO           ?= go
GOTEST_FLAGS ?=
BIN          := bin/catalog
IMAGE        ?= agrogram/catalog:dev
DOCKER       ?= docker

# Loads ./.env (git-ignored) into the environment when it exists.
LOAD_ENV := set -a; if [ -f ./.env ]; then . ./.env; fi; set +a;

# The catalog stack joins the infra project; both files are needed, and
# Compose reads back/deploy/.env because the first file lives there.
COMPOSE := cd $(BACK_DIR) && $(DOCKER) compose -f deploy/docker-compose.yml -f services/catalog/compose.yaml

.PHONY: help tools gen gen-check tidy build run test test-race test-integration \
	migrate-up migrate-down migrate-status lint docker-build infra-env-check \
	compose-up compose-down compose-logs clean

help: ## List targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

tools: ## Download and verify the pinned buf binary (cached outside the repo)
	@$(CODEGEN) tools

gen: ## Generate back/gen (Go, Python, Java) with the pinned buf; write gen/go/go.mod
	$(CODEGEN) generate

gen-check:
	@test -f $(GEN_GO_DIR)/go.mod || { \
		echo "$(GEN_GO_DIR)/go.mod is missing. Generate the code first: make gen" >&2; \
		echo "(or, from back/: 'make proto-gen-local', or 'make proto-gen' for the Docker path.)" >&2; \
		exit 1; }

tidy: gen-check ## go mod tidy
	$(GO) mod tidy

build: gen-check ## Build bin/catalog
	$(GO) build -trimpath -o $(BIN) ./cmd/catalog

run: gen-check ## Run the service locally: gRPC, HTTP health, outbox relay, view counter (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/catalog serve

test: gen-check ## Unit tests (no infrastructure needed)
	$(GO) test $(GOTEST_FLAGS) ./...

test-race: gen-check ## Unit tests with the race detector (needs cgo and a C compiler)
	CGO_ENABLED=1 $(GO) test -race $(GOTEST_FLAGS) ./...

test-integration: gen-check ## Integration tests against CATALOG_TEST_DATABASE_URL (loads ./.env; temp schema)
	$(LOAD_ENV) $(GO) test -tags=integration -count=1 $(GOTEST_FLAGS) ./...

migrate-up: gen-check ## Apply all pending migrations, product seed included (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/catalog migrate up

migrate-down: gen-check ## Roll back the latest migration (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/catalog migrate down

migrate-status: gen-check ## Show migration status (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/catalog migrate status

lint: gen-check ## go vet (with and without the integration tag) and a gofmt check
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...
	@out="$$("$$($(GO) env GOROOT)/bin/gofmt" -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:" >&2; echo "$$out" >&2; exit 1; fi; \
	echo "gofmt: clean"

docker-build: gen-check ## Build the image IMAGE (context back/)
	cd $(BACK_DIR) && $(DOCKER) build -f services/catalog/Dockerfile -t $(IMAGE) .

infra-env-check:
	@test -f $(BACK_DIR)/deploy/.env || { \
		echo "back/deploy/.env not found. From back/: cp deploy/.env.example deploy/.env" >&2; exit 1; }

compose-up: gen-check infra-env-check ## Build and start catalog (+ migrations) with the infra stack; wait until healthy
	$(COMPOSE) up --detach --build --wait --wait-timeout 300 catalog

compose-down: infra-env-check ## Stop and remove the catalog containers (infra keeps running)
	$(COMPOSE) rm --stop --force catalog catalog-migrate

compose-logs: infra-env-check ## Follow catalog and migration logs
	$(COMPOSE) logs --follow --tail 200 catalog-migrate catalog

clean: ## Remove build outputs
	rm -rf bin coverage.out
