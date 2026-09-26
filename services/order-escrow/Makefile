# order-escrow: the escrow transaction (tranzaksiya) service. Java 17 + Spring
# Boot, built with the Gradle wrapper (./gradlew downloads the pinned Gradle).
#
# Run every target from this directory (back/services/order-escrow).
#
#   gen       delegates to the shared tooling, back/tools/codegen.sh: it needs
#             curl and network access only, downloads the pinned buf release
#             binary into a user cache OUTSIDE the repo, verifies it against the
#             release's sha256.txt and runs `buf generate` from back/ with
#             back/buf.gen.yaml; the Java code lands in back/gen/java, which the
#             build compiles as its `contracts` source set. `make proto-gen-local`
#             in back/ runs exactly the same thing.
#   Java      targets need a JDK 17 or newer: java on PATH, or JAVA_HOME. The
#             wrapper downloads Gradle and the dependencies (~/.gradle).
#   run, migrate-*, test-integration
#             load ./.env when it exists (cp .env.example .env).
#   docker-*, compose-*
#             run Docker; the compose targets layer compose.yaml on top of
#             back/deploy/docker-compose.yml (see README.md).

SHELL := /bin/sh
.DEFAULT_GOAL := help

BACK_DIR := ../..

# Shared codegen tooling: defines CODEGEN (back/tools/codegen.sh) and the single
# pinned BUF_VERSION. Override with BUF_VERSION=, TOOLS_DIR= (the binary cache)
# or BUF=<path to an existing buf>.
include $(BACK_DIR)/tools/codegen.mk

GEN_JAVA_DIR := $(BACK_DIR)/gen/java
GRADLE       ?= ./gradlew
GRADLE_FLAGS ?=
JAVA         ?= $(if $(JAVA_HOME),$(JAVA_HOME)/bin/java,java)
JAR          := build/libs/order-escrow.jar
IMAGE        ?= agrogram/order-escrow:dev
DOCKER       ?= docker

# Loads ./.env (git-ignored) into the environment when it exists.
LOAD_ENV := set -a; if [ -f ./.env ]; then . ./.env; fi; set +a;

# The order-escrow stack joins the infra project; both files are needed, and
# Compose reads back/deploy/.env because the first file lives there.
COMPOSE := cd $(BACK_DIR) && $(DOCKER) compose -f deploy/docker-compose.yml -f services/order-escrow/compose.yaml

.PHONY: help tools gen gen-check build run test test-integration check lint \
	migrate-up migrate-down migrate-status docker-build infra-env-check \
	compose-up compose-down compose-logs clean

help: ## List targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

tools: ## Download and verify the pinned buf binary (cached outside the repo)
	@$(CODEGEN) tools

gen: ## Generate back/gen (Go, Python, Java) with the pinned buf
	$(CODEGEN) generate

gen-check:
	@test -d $(GEN_JAVA_DIR)/uz/agrogram/orderescrow/v1 || { \
		echo "$(GEN_JAVA_DIR) has no order-escrow code. Generate it first: make gen" >&2; \
		echo "(or, from back/: 'make proto-gen-local', or 'make proto-gen' for the Docker path.)" >&2; \
		exit 1; }

build: gen-check ## Build the executable jar build/libs/order-escrow.jar
	$(GRADLE) $(GRADLE_FLAGS) bootJar

run: build ## Run the service locally: gRPC, HTTP health, outbox relay (loads ./.env)
	$(LOAD_ENV) $(JAVA) -jar $(JAR) serve

test: gen-check ## Unit tests (no infrastructure needed)
	$(GRADLE) $(GRADLE_FLAGS) test

test-integration: gen-check ## Integration tests against ORDER_ESCROW_TEST_DATABASE_URL (loads ./.env; temp schema)
	$(LOAD_ENV) $(GRADLE) $(GRADLE_FLAGS) integrationTest

lint: gen-check ## javac lint as errors (-Xlint -Werror) on main and test code
	$(GRADLE) $(GRADLE_FLAGS) compileJava compileTestJava

check: gen-check ## lint + unit tests (Gradle's check)
	$(GRADLE) $(GRADLE_FLAGS) check

migrate-up: build ## Apply all pending migrations (loads ./.env)
	$(LOAD_ENV) $(JAVA) -jar $(JAR) migrate up

migrate-down: build ## Roll back the latest migration with its db/down script (loads ./.env)
	$(LOAD_ENV) $(JAVA) -jar $(JAR) migrate down

migrate-status: build ## Show migration status (loads ./.env)
	$(LOAD_ENV) $(JAVA) -jar $(JAR) migrate status

docker-build: gen-check ## Build the image IMAGE (context back/)
	cd $(BACK_DIR) && $(DOCKER) build -f services/order-escrow/Dockerfile -t $(IMAGE) .

infra-env-check:
	@test -f $(BACK_DIR)/deploy/.env || { \
		echo "back/deploy/.env not found. From back/: cp deploy/.env.example deploy/.env" >&2; exit 1; }

compose-up: gen-check infra-env-check ## Build and start order-escrow (+ migrations) with the infra stack; wait until healthy
	$(COMPOSE) up --detach --build --wait --wait-timeout 300 order-escrow

compose-down: infra-env-check ## Stop and remove the order-escrow containers (infra keeps running)
	$(COMPOSE) rm --stop --force order-escrow order-escrow-migrate

compose-logs: infra-env-check ## Follow order-escrow and migration logs
	$(COMPOSE) logs --follow --tail 200 order-escrow-migrate order-escrow

clean: ## Remove build outputs
	$(GRADLE) $(GRADLE_FLAGS) clean
