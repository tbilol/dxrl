# auth-gateway: sign-in, sessions, token issue and verify, JWKS.
#
# Run every target from this directory (back/services/auth-gateway).
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
#   dev-key   generates the local ES256 signing key (secrets/, git-ignored).
#             Nothing starts without a key; the service never invents one.
#   run, migrate-*, test-integration
#             load ./.env when it exists (cp .env.example .env).
#   docker-*, compose-*
#             run Docker; the compose targets layer compose.yaml on top of
#             back/deploy/docker-compose.yml AND services/identity/compose.yaml,
#             because auth-gateway calls identity (see README.md).

SHELL := /bin/sh
.DEFAULT_GOAL := help

BACK_DIR := ../..

# Shared codegen tooling: defines CODEGEN (back/tools/codegen.sh), GEN_GO_DIR
# and the single pinned BUF_VERSION. Override with BUF_VERSION=, TOOLS_DIR= (the
# binary cache) or BUF=<path to an existing buf>.
include $(BACK_DIR)/tools/codegen.mk

GO           ?= go
GOTEST_FLAGS ?=
BIN          := bin/auth-gateway
IMAGE        ?= agrogram/auth-gateway:dev
DOCKER       ?= docker
JWT_KEY      ?= secrets/jwt-es256.pem

# Loads ./.env (git-ignored) into the environment when it exists.
LOAD_ENV := set -a; if [ -f ./.env ]; then . ./.env; fi; set +a;

# The auth-gateway stack joins the infra project and needs identity; all three
# files are needed, and Compose reads back/deploy/.env because the first file
# lives there.
COMPOSE := cd $(BACK_DIR) && $(DOCKER) compose \
	-f deploy/docker-compose.yml \
	-f services/identity/compose.yaml \
	-f services/auth-gateway/compose.yaml

.PHONY: help tools gen gen-check tidy build run test test-race test-integration \
	dev-key migrate-up migrate-down migrate-status lint docker-build infra-env-check \
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

build: gen-check ## Build bin/auth-gateway
	$(GO) build -trimpath -o $(BIN) ./cmd/auth-gateway

run: gen-check ## Run the service locally (loads ./.env; needs a JWT key, see dev-key)
	$(LOAD_ENV) $(GO) run ./cmd/auth-gateway serve

dev-key: ## Generate the local ES256 signing key into secrets/ (git-ignored)
	@set -eu; \
	if [ -f "$(JWT_KEY)" ]; then echo "$(JWT_KEY) exists; delete it first to rotate"; exit 0; fi; \
	mkdir -p "$$(dirname "$(JWT_KEY)")"; \
	umask 077; \
	openssl ecparam -name prime256v1 -genkey -noout -out "$(JWT_KEY)"; \
	chmod 600 "$(JWT_KEY)"; \
	echo "Wrote $(JWT_KEY) (ES256, P-256). Point AUTH_GATEWAY_JWT_PRIVATE_KEY_FILE at it."

test: gen-check ## Unit tests (no infrastructure needed)
	$(GO) test $(GOTEST_FLAGS) ./...

test-race: gen-check ## Unit tests with the race detector (needs cgo and a C compiler)
	CGO_ENABLED=1 $(GO) test -race $(GOTEST_FLAGS) ./...

test-integration: gen-check ## Integration tests against AUTH_GATEWAY_TEST_DATABASE_URL (loads ./.env; temp schema)
	$(LOAD_ENV) $(GO) test -tags=integration -count=1 $(GOTEST_FLAGS) ./...

migrate-up: gen-check ## Apply all pending migrations (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/auth-gateway migrate up

migrate-down: gen-check ## Roll back the latest migration (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/auth-gateway migrate down

migrate-status: gen-check ## Show migration status (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/auth-gateway migrate status

lint: gen-check ## go vet (with and without the integration tag) and a gofmt check
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...
	@out="$$("$$($(GO) env GOROOT)/bin/gofmt" -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:" >&2; echo "$$out" >&2; exit 1; fi; \
	echo "gofmt: clean"

docker-build: gen-check ## Build the image IMAGE (context back/)
	cd $(BACK_DIR) && $(DOCKER) build -f services/auth-gateway/Dockerfile -t $(IMAGE) .

infra-env-check:
	@test -f $(BACK_DIR)/deploy/.env || { \
		echo "back/deploy/.env not found. From back/: cp deploy/.env.example deploy/.env" >&2; exit 1; }
	@test -f $(JWT_KEY) || { \
		echo "$(JWT_KEY) not found; the container mounts secrets/ read-only. Run: make dev-key" >&2; exit 1; }

compose-up: gen-check infra-env-check ## Build and start auth-gateway (+ identity, + migrations) with the infra stack
	$(COMPOSE) up --detach --build --wait --wait-timeout 300 auth-gateway

compose-down: infra-env-check ## Stop and remove the auth-gateway containers (infra and identity keep running)
	$(COMPOSE) rm --stop --force auth-gateway auth-gateway-migrate

compose-logs: infra-env-check ## Follow auth-gateway and migration logs
	$(COMPOSE) logs --follow --tail 200 auth-gateway-migrate auth-gateway

clean: ## Remove build outputs
	rm -rf bin coverage.out
