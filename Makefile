SHELL := /bin/sh

# Include staged, unstaged and untracked changes; dirty builds are never
# advertised as exact clean commits. Direct Compose defaults to unknown.
BUILD_REVISION := $(shell git rev-parse --verify HEAD 2>/dev/null || echo unknown)$(shell test -z "$$(git status --porcelain 2>/dev/null)" || echo -dirty)
export BUILD_REVISION

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

.PHONY: help config build up down destroy restart ps logs migrate migrate-down test openapi-check integration-test queue-benchmark vet fmt fmt-check tidy db-shell activity-up activity-embedded activity-test activity-ci activity-backup-verify activity-transfer-verify

.PHONY: activity-observability-test
PROMETHEUS_IMAGE ?= prom/prometheus:v2.55.1

ACTIVITY_COMPOSE := $(COMPOSE) -p amocrm-activity -f docker-compose.activity.yml
ACTIVITY_TEST_PROJECT ?= amocrm-pro-activity-test
# Explicit -p wins over both the development file name and an inherited
# COMPOSE_PROJECT_NAME. Test cleanup must never target the pilot cluster.
ACTIVITY_TEST_COMPOSE := $(COMPOSE) -p $(ACTIVITY_TEST_PROJECT) --profile tests -f docker-compose.activity.yml -f docker-compose.activity-tests.yml

activity-up: ## Start isolated development Activity in separate gRPC processes
	$(ACTIVITY_COMPOSE) up --build --detach

activity-embedded: ## Start embedded Activity graph (stop remote products first; see runbook)
	$(ACTIVITY_COMPOSE) -f docker-compose.activity-embedded.yml up --build --detach

activity-test: ## Run owner DB, mTLS, process and UI checks in a separate test project
	@case "$(ACTIVITY_TEST_PROJECT)" in *-test) ;; *) echo "Refusing non-test project: use a dedicated name ending in -test" >&2; exit 1;; esac
	mkdir -p tmp/activity-v0-evidence
	$(ACTIVITY_TEST_COMPOSE) up --detach --wait postgres
	$(ACTIVITY_TEST_COMPOSE) exec -T postgres sh < deploy/activity/init-tests.sh
	$(ACTIVITY_TEST_COMPOSE) build migrate-test-core component-tests
	$(ACTIVITY_TEST_COMPOSE) run --rm migrate-test-core up
	$(ACTIVITY_TEST_COMPOSE) run --rm --no-deps component-tests
	$(ACTIVITY_TEST_COMPOSE) run --rm --no-deps component-ui-tests

activity-backup-verify: ## Synthetic dump/restore of Core, Activity and CRM Events owner DBs
	sh deploy/activity/verify-backup-owners.sh --isolated

activity-transfer-verify: ## Overlay config plus Events restore onto a second PostgreSQL
	sh deploy/activity/verify-transfer.sh

activity-observability-test: ## Validate Prometheus config and exercise pilot alert rules
	$(DOCKER) run --rm --entrypoint /bin/promtool -v "$(CURDIR)/deploy/observability:/config:ro" $(PROMETHEUS_IMAGE) check config /config/prometheus.yml
	$(DOCKER) run --rm --entrypoint /bin/promtool -v "$(CURDIR)/deploy/observability:/config:ro" $(PROMETHEUS_IMAGE) test rules /config/alerts.test.yml

activity-ci: activity-observability-test activity-backup-verify ## Run Activity verification with disposable PostgreSQL and automatic cleanup
	@set -eu; \
	case "$(ACTIVITY_TEST_PROJECT)" in *-test) ;; *) echo "Refusing non-test project: use a dedicated name ending in -test" >&2; exit 1;; esac; \
	mkdir -p tmp/activity-v0-evidence; \
	cleanup() { $(ACTIVITY_TEST_COMPOSE) down --volumes --remove-orphans >tmp/activity-v0-evidence/compose-cleanup.log 2>&1 || true; }; \
	trap cleanup EXIT INT TERM; \
	cleanup; \
	$(MAKE) COMPOSE="$(COMPOSE)" ACTIVITY_TEST_PROJECT="$(ACTIVITY_TEST_PROJECT)" activity-test

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
