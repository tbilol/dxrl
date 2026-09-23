# identity: accounts, role set, e-ijara farmer verification and plan.
#
# Run every target from this directory (back/services/identity).
#
#   gen       needs curl and network access only: it downloads the pinned buf
#             release binary into a user cache OUTSIDE the repo, verifies it
#             against the release's sha256.txt, runs `buf generate` from back/
#             with back/buf.gen.yaml (remote BSR plugins), and writes
#             back/gen/go/go.mod so the generated Go code is a module.
#   Go        targets need a Go toolchain; go.mod selects the version and
#             GOTOOLCHAIN=auto (the default) downloads it.
#   run, migrate-*, test-integration
#             load ./.env when it exists (cp .env.example .env).
#   docker-*, compose-*
#             run Docker; the compose targets layer compose.yaml on top of
#             back/deploy/docker-compose.yml (see README.md).

SHELL := /bin/sh
.DEFAULT_GOAL := help

BACK_DIR   := ../..
GEN_GO_DIR := $(BACK_DIR)/gen/go

# Same version back/Makefile pins for its Docker-based buf.
BUF_VERSION ?= 1.73.0
TOOLS_DIR   ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/agrogram/bin
BUF         := $(TOOLS_DIR)/buf-$(BUF_VERSION)

# Release assets are buf-<uname -s>-<arch>: Linux-x86_64, Linux-aarch64,
# Darwin-x86_64, Darwin-arm64.
UNAME_S  := $(shell uname -s)
UNAME_M  := $(shell uname -m)
BUF_ARCH := $(UNAME_M)
ifeq ($(UNAME_S)-$(UNAME_M),Linux-arm64)
BUF_ARCH := aarch64
endif
ifeq ($(UNAME_S)-$(UNAME_M),Darwin-aarch64)
BUF_ARCH := arm64
endif
BUF_ASSET := buf-$(UNAME_S)-$(BUF_ARCH)
BUF_URL   := https://github.com/bufbuild/buf/releases/download/v$(BUF_VERSION)

GO           ?= go
GOTEST_FLAGS ?=
BIN          := bin/identity
IMAGE        ?= agrogram/identity:dev
DOCKER       ?= docker

# Loads ./.env (git-ignored) into the environment when it exists.
LOAD_ENV := set -a; if [ -f ./.env ]; then . ./.env; fi; set +a;

# The identity stack joins the infra project; both files are needed, and
# Compose reads back/deploy/.env because the first file lives there.
COMPOSE := cd $(BACK_DIR) && $(DOCKER) compose -f deploy/docker-compose.yml -f services/identity/compose.yaml

.PHONY: help tools gen gen-check tidy build run test test-race test-integration \
	migrate-up migrate-down migrate-status lint docker-build infra-env-check \
	compose-up compose-down compose-logs clean

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

tools: $(BUF) ## Download and verify the pinned buf binary (cached outside the repo)
	@"$(BUF)" --version

$(BUF):
	@set -eu; \
	mkdir -p "$(TOOLS_DIR)"; \
	tmp="$$(mktemp -d "$(TOOLS_DIR)/.buf-download.XXXXXX")"; \
	trap 'rm -rf "$$tmp"' EXIT INT TERM; \
	echo "Downloading buf $(BUF_VERSION) ($(BUF_ASSET)) into $(TOOLS_DIR)"; \
	curl -fsSL --retry 3 -o "$$tmp/buf" "$(BUF_URL)/$(BUF_ASSET)"; \
	curl -fsSL --retry 3 -o "$$tmp/sha256.txt" "$(BUF_URL)/sha256.txt"; \
	want="$$(awk -v f="$(BUF_ASSET)" '$$2 == f {print $$1}' "$$tmp/sha256.txt")"; \
	if [ -z "$$want" ]; then echo "sha256.txt has no entry for $(BUF_ASSET)" >&2; exit 1; fi; \
	if command -v sha256sum >/dev/null 2>&1; then got="$$(sha256sum "$$tmp/buf" | awk '{print $$1}')"; \
	else got="$$(shasum -a 256 "$$tmp/buf" | awk '{print $$1}')"; fi; \
	if [ "$$got" != "$$want" ]; then echo "checksum mismatch for $(BUF_ASSET): got $$got, want $$want" >&2; exit 1; fi; \
	chmod +x "$$tmp/buf"; \
	mv "$$tmp/buf" "$@"; \
	echo "buf $(BUF_VERSION) OK (sha256 $$got)"

gen: $(BUF) ## Generate back/gen (Go, Python, Java) with the pinned buf; write gen/go/go.mod
	cd $(BACK_DIR) && "$(BUF)" generate
	cp gen-go.mod.tmpl $(GEN_GO_DIR)/go.mod
	@echo "Wrote $(GEN_GO_DIR)/go.mod (module github.com/tbilol/dxrl/gen/go)"

gen-check:
	@test -f $(GEN_GO_DIR)/go.mod || { \
		echo "$(GEN_GO_DIR)/go.mod is missing. Generate the code first: make gen" >&2; \
		echo "(back/'s 'make proto-gen' wipes gen/ and does not write it; re-run 'make gen' here after it.)" >&2; \
		exit 1; }

tidy: gen-check ## go mod tidy
	$(GO) mod tidy

build: gen-check ## Build bin/identity
	$(GO) build -trimpath -o $(BIN) ./cmd/identity

run: gen-check ## Run the service locally (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/identity serve

test: gen-check ## Unit tests (no infrastructure needed)
	$(GO) test $(GOTEST_FLAGS) ./...

test-race: gen-check ## Unit tests with the race detector (needs cgo and a C compiler)
	CGO_ENABLED=1 $(GO) test -race $(GOTEST_FLAGS) ./...

test-integration: gen-check ## Integration tests against IDENTITY_TEST_DATABASE_URL (loads ./.env; temp schema)
	$(LOAD_ENV) $(GO) test -tags=integration -count=1 $(GOTEST_FLAGS) ./...

migrate-up: gen-check ## Apply all pending migrations (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/identity migrate up

migrate-down: gen-check ## Roll back the latest migration (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/identity migrate down

migrate-status: gen-check ## Show migration status (loads ./.env)
	$(LOAD_ENV) $(GO) run ./cmd/identity migrate status

lint: gen-check ## go vet (with and without the integration tag) and a gofmt check
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...
	@out="$$("$$($(GO) env GOROOT)/bin/gofmt" -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:" >&2; echo "$$out" >&2; exit 1; fi; \
	echo "gofmt: clean"

docker-build: gen-check ## Build the image IMAGE (context back/)
	cd $(BACK_DIR) && $(DOCKER) build -f services/identity/Dockerfile -t $(IMAGE) .

infra-env-check:
	@test -f $(BACK_DIR)/deploy/.env || { \
		echo "back/deploy/.env not found. From back/: cp deploy/.env.example deploy/.env" >&2; exit 1; }

compose-up: gen-check infra-env-check ## Build and start identity (+ migrations) with the infra stack; wait until healthy
	$(COMPOSE) up --detach --build --wait --wait-timeout 300 identity

compose-down: infra-env-check ## Stop and remove the identity containers (infra keeps running)
	$(COMPOSE) rm --stop --force identity identity-migrate

compose-logs: infra-env-check ## Follow identity and migration logs
	$(COMPOSE) logs --follow --tail 200 identity-migrate identity

clean: ## Remove build outputs
	rm -rf bin coverage.out
