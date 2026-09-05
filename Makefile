SHELL := /bin/sh

DOCKER ?= docker
COMPOSE ?= docker-compose
GO_VERSION ?= 1.25
GO_IMAGE ?= golang:$(GO_VERSION)-alpine
TEST_COMPOSE_PROJECT ?= amocrm-pro-integration-test
TEST_COMPOSE := COMPOSE_PROJECT_NAME=$(TEST_COMPOSE_PROJECT) $(COMPOSE) -f docker-compose.test.yml
MIGRATION_COUNT := $(words $(wildcard migrations/*.up.sql))
QUEUE_BENCHMARK_PROJECT ?= amocrm-pro-queue-benchmark
QUEUE_BENCHMARK_COMPOSE := COMPOSE_PROJECT_NAME=$(QUEUE_BENCHMARK_PROJECT) $(COMPOSE) -f docker-compose.test.yml
QUEUE_BENCHMARK_OUTPUT ?= $(CURDIR)/tmp/queue-benchmark
QUEUE_BENCHMARK_SAMPLES ?= 12
QUEUE_BENCHMARK_CASES ?=

UID := $(shell id -u)
GID := $(shell id -g)
DOCKER_GO := $(DOCKER) run --rm \
	--user "$(UID):$(GID)" \
	--env HOME=/tmp \
	--env GOCACHE=/tmp/go-build \
	--env GOMODCACHE=/tmp/go/pkg/mod \
	--volume "$(CURDIR):/src" \
	--workdir /src \
	$(GO_IMAGE)

.DEFAULT_GOAL := help

.PHONY: help config build up down destroy restart ps logs migrate migrate-down test openapi-check integration-test queue-benchmark vet fmt fmt-check tidy db-shell

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

config: ## Validate the resolved Docker Compose configuration
	$(COMPOSE) config --quiet

build: ## Build API, worker, migration, and operator images
	$(COMPOSE) build api worker migrate integrations

up: ## Build and start the complete local stack
	$(COMPOSE) up --build --detach

down: ## Stop the local stack and remove its containers
	$(COMPOSE) down --remove-orphans

destroy: ## Stop the stack and delete its local PostgreSQL volume
	$(COMPOSE) down --volumes --remove-orphans

restart: down up ## Recreate the local stack

ps: ## Show local service status
	$(COMPOSE) ps

logs: ## Follow API and worker logs
	$(COMPOSE) logs --follow api worker

migrate: ## Apply pending PostgreSQL migrations
	$(COMPOSE) run --rm migrate up

migrate-down: ## Revert all migrations (requires MIGRATION_DOWN_CONFIRM=revert-all-migrations)
	@test "$(MIGRATION_DOWN_CONFIRM)" = "revert-all-migrations" || { \
		echo "Refusing destructive rollback; follow docs/runbooks/migrate-down.md" >&2; exit 1; \
	}
	$(COMPOSE) run --rm -e MIGRATION_DOWN_CONFIRM="$(MIGRATION_DOWN_CONFIRM)" migrate down

test: ## Run formatting checks, vet, and race-enabled tests in Docker
	$(DOCKER) build --build-arg GO_VERSION=$(GO_VERSION) --target test .

openapi-check: ## Validate the OpenAPI contract in Docker
	$(DOCKER) build --build-arg GO_VERSION=$(GO_VERSION) --target openapi-test .

integration-test: ## Run migrations and PostgreSQL integration tests in an isolated Docker stack
	@set -eu; \
	cleanup() { $(TEST_COMPOSE) down --volumes --remove-orphans >/dev/null 2>&1 || true; }; \
	trap cleanup EXIT INT TERM; \
	cleanup; \
	$(TEST_COMPOSE) build migrate integration-test; \
	$(TEST_COMPOSE) up --detach postgres; \
	$(TEST_COMPOSE) run --rm migrate up; \
	$(TEST_COMPOSE) exec -T postgres psql -U amocrm_test -d amocrm_test -Atc "SELECT count(*) FROM schema_migrations WHERE octet_length(checksum)=32 AND octet_length(down_checksum)=32" | grep -qx '$(MIGRATION_COUNT)'; \
	if $(TEST_COMPOSE) run --rm --no-deps migrate down; then echo "unconfirmed migrate down unexpectedly succeeded" >&2; exit 1; fi; \
	$(TEST_COMPOSE) exec -T postgres psql -U amocrm_test -d amocrm_test -Atc "SELECT count(*) = $(MIGRATION_COUNT) AND to_regclass('public.jobs') IS NOT NULL FROM schema_migrations" | grep -qx 't'; \
	$(TEST_COMPOSE) run --rm --no-deps -e MIGRATION_DOWN_CONFIRM=revert-all-migrations migrate down; \
	$(TEST_COMPOSE) exec -T postgres psql -U amocrm_test -d amocrm_test -Atc "SELECT to_regclass('public.jobs') IS NULL" | grep -qx 't'; \
	$(TEST_COMPOSE) run --rm --no-deps migrate up & first=$$!; \
	$(TEST_COMPOSE) run --rm --no-deps migrate up & second=$$!; \
	wait $$first; \
	wait $$second; \
	$(TEST_COMPOSE) exec -T postgres psql -U amocrm_test -d amocrm_test -Atc "SELECT count(*) = $(MIGRATION_COUNT) AND to_regclass('public.jobs') IS NOT NULL FROM schema_migrations" | grep -qx 't'; \
	$(TEST_COMPOSE) run --rm --no-deps integration-test

queue-benchmark: ## Measure fair claims and a small worker drain in an isolated PostgreSQL stack
	@set -eu; \
	cleanup() { $(QUEUE_BENCHMARK_COMPOSE) down --volumes --remove-orphans >/dev/null 2>&1 || true; }; \
	trap cleanup EXIT INT TERM; \
	cleanup; \
	mkdir -p "$(QUEUE_BENCHMARK_OUTPUT)"; \
	$(QUEUE_BENCHMARK_COMPOSE) build migrate integration-test; \
	$(QUEUE_BENCHMARK_COMPOSE) up --detach postgres; \
	$(QUEUE_BENCHMARK_COMPOSE) run --rm migrate up; \
	$(QUEUE_BENCHMARK_COMPOSE) run --rm --no-deps \
		-v "$(QUEUE_BENCHMARK_OUTPUT):/output" \
		-e FAIR_CLAIM_BENCHMARK=true \
		-e FAIR_CLAIM_BENCHMARK_OUTPUT=/output \
		-e FAIR_CLAIM_BENCHMARK_SAMPLES="$(QUEUE_BENCHMARK_SAMPLES)" \
		-e FAIR_CLAIM_BENCHMARK_CASES="$(QUEUE_BENCHMARK_CASES)" \
		integration-test -count=1 -timeout=30m -v -run '^(TestFairClaimPerformance|TestWorkerPollingPerformance)$$' ./internal/jobs

vet: ## Run go vet in Docker
	$(DOCKER_GO) go vet ./...

fmt: ## Format Go sources in Docker
	$(DOCKER_GO) gofmt -w .

fmt-check: ## Check Go formatting in Docker
	$(DOCKER_GO) sh -ec 'files="$$(gofmt -l .)"; if [ -n "$$files" ]; then printf "%s\n" "$$files"; exit 1; fi'

tidy: ## Run go mod tidy in Docker
	$(DOCKER_GO) go mod tidy

db-shell: ## Open psql in the PostgreSQL container
	$(COMPOSE) exec postgres psql -U "$${POSTGRES_USER:-amocrm}" -d "$${POSTGRES_DB:-amocrm}"
