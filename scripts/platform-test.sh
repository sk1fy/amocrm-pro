#!/bin/sh
set -eu

mkdir -p tmp
test_dir=$(mktemp -d "$(pwd)/tmp/platform-test.XXXXXX")
project=$(basename "$test_dir" | tr '[:upper:].' '[:lower:]-')
export PLATFORM_DEV_CERTS_DIR="$test_dir/certs"
export PLATFORM_UID=$(id -u) PLATFORM_GID=$(id -g)
export PLATFORM_PUBLIC_PORT=0 PLATFORM_PRIVATE_PORT=0
export PLATFORM_JOBS_POOL_MAX_CONNS=1 PLATFORM_MANAGEMENT_HTTP_ADDRESS=:8081
export PLATFORM_MANAGER_CURSOR_KEY=

compose() {
    ${COMPOSE:-docker-compose} -p "$project" --profile tests -f docker-compose.platform.yml "$@"
}
cleanup() {
    compose down --volumes --remove-orphans
    rm -rf "$test_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

make platform-certs
compose build platform platform-migrate platform-test
compose up --detach --wait postgres

expect_start_failure() {
    if compose run --rm --no-deps "$@" platform >"$test_dir/start.log" 2>&1; then
        cat "$test_dir/start.log"
        echo "Platform started with an invalid database configuration" >&2
        exit 1
    fi
    if ! grep -F "$start_error" "$test_dir/start.log"; then
        cat "$test_dir/start.log"
        exit 1
    fi
}

start_error="run Platform migrations and verify access role grants"
expect_start_failure
compose run --rm --no-deps platform-migrate &
first=$!
compose run --rm --no-deps platform-migrate &
second=$!
wait "$first"
wait "$second"

migration_ledger() {
    compose exec -T postgres psql -U postgres -d amocrm_platform -Atc \
        "SELECT version, encode(checksum, 'hex'), encode(down_checksum, 'hex'), applied_at FROM schema_migrations ORDER BY version"
}
original_ledger=$(migration_ledger)
printf '%s\n' "$original_ledger" | grep -Eq '^000001_runtime\.up\.sql\|[0-9a-f]{64}\|[0-9a-f]{64}\|[^|]+$'
set -- migrations-platform/*.up.sql
test "$(printf '%s\n' "$original_ledger" | wc -l | tr -d ' ')" -eq "$#"
compose run --rm --no-deps platform-migrate
test "$original_ledger" = "$(migration_ledger)"
compose exec -T postgres psql -U postgres -d amocrm_platform -Atc "SELECT to_regclass('public.jobs') IS NULL" | grep -qx t

start_error="open jobs pool"
expect_start_failure -e PLATFORM_JOBS_DATABASE_URL=postgres://platform_access:platform_access_dev@postgres:5432/amocrm_platform?sslmode=disable
start_error="open access pool"
expect_start_failure -e PLATFORM_ACCESS_DATABASE_URL=postgres://platform_access:platform_access_dev@127.0.0.1:1/amocrm_platform?sslmode=disable

term_grant() {
    compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U platform_owner -d amocrm_platform -c "$1"
}
term_grant 'REVOKE UPDATE (entitlement_version) ON platform_lifecycle.terms FROM platform_jobs'
start_error="manager mutation grants are unavailable"
expect_start_failure -e PLATFORM_MANAGER_CURSOR_KEY=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8
term_grant 'GRANT UPDATE (entitlement_version) ON platform_lifecycle.terms TO platform_jobs'

compose up --detach --wait --no-deps platform
compose run --rm --no-deps platform-test go test -race -count=1 -v -run '^TestPlatformProcess$' ./internal/productplatform
compose stop platform
test "$(compose ps -a --format '{{.State}}:{{.ExitCode}}' platform)" = "exited:0"
compose run --rm --no-deps platform-test go test -race -count=1 -v -run '^TestQueueRuntime$' ./internal/productplatform/queue

compose run --rm --no-deps platform-test sh -ceu '
    go build -race -o /tmp/platform-manager-test ./cmd/platform
    PLATFORM_TEST_BINARY=/tmp/platform-manager-test go test -race -count=1 -v -run "^TestPlatform((Manager|Accounts|AccountProfile|Portrait|Products)AcrossReplicas|ManualRenewal|ProductCreateReplayAfterBootstrap)$" ./internal/productplatform
'
