#!/bin/sh
set -eu

docker_cmd=${DOCKER:-docker}
mkdir -p tmp
work_dir=$(mktemp -d "$(pwd)/tmp/platform-lifecycle-test.XXXXXX")
project=$(basename "$work_dir" | tr '[:upper:].' '[:lower:]-')
core="$project-core"
platform="$project-platform"
runner="$project-runner"

cleanup() {
    "$docker_cmd" rm -f "$runner" "$platform" "$core" >/dev/null 2>&1 || true
    "$docker_cmd" network rm "$project" >/dev/null 2>&1 || true
    "$docker_cmd" image rm "$project" >/dev/null 2>&1 || true
    rmdir "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$docker_cmd" build --target test-base --build-arg "GO_VERSION=${GO_VERSION:-1.26.11}" -t "$project" .
"$docker_cmd" network create "$project" >/dev/null
"$docker_cmd" run -d --name "$core" --network "$project" --network-alias core-db \
    -e POSTGRES_DB=amocrm_test -e POSTGRES_USER=amocrm -e POSTGRES_PASSWORD=core_db_test postgres:17-alpine >/dev/null
"$docker_cmd" run -d --name "$platform" --network "$project" --network-alias platform-db \
    -e POSTGRES_PASSWORD=platform_postgres_dev \
    -v "$(pwd)/deploy/platform/init.sql:/docker-entrypoint-initdb.d/10-platform.sql:ro" postgres:17-alpine >/dev/null

wait_db() {
    attempts=30
    until "$docker_cmd" exec "$1" pg_isready -h 127.0.0.1 -U "$2" -d "$3" >/dev/null 2>&1; do
        attempts=$((attempts - 1))
        if [ "$attempts" -eq 0 ]; then
            "$docker_cmd" logs "$1" >&2
            return 1
        fi
        sleep 1
    done
}
wait_db "$core" amocrm amocrm_test
wait_db "$platform" platform_owner amocrm_platform

"$docker_cmd" run --rm --name "$runner" --network "$project" \
    -e TEST_DATABASE_URL=postgres://amocrm:core_db_test@core-db:5432/amocrm_test?sslmode=disable \
    -e TEST_DATABASE_RESET_ALLOWED=true \
    -e PLATFORM_TEST_OWNER_DSN=postgres://platform_owner:platform_owner_dev@platform-db:5432/amocrm_platform?sslmode=disable \
    -e PLATFORM_TEST_JOBS_DSN=postgres://platform_jobs:platform_jobs_dev@platform-db:5432/amocrm_platform?sslmode=disable \
    -e PLATFORM_TEST_CERTS_DIR=/tmp/platform-lifecycle-certs \
    "$project" sh -ceu '
        DATABASE_URL="$TEST_DATABASE_URL" MIGRATIONS_DIR=/src/migrations go run ./cmd/migrate up
        DATABASE_URL="$PLATFORM_TEST_OWNER_DSN" MIGRATION_TARGET=platform MIGRATIONS_DIR=/src/migrations-platform go run ./cmd/migrate up
        go run ./cmd/service-certs --platform "$PLATFORM_TEST_CERTS_DIR"
        go test -race -count=1 -v ./internal/corelifecycle
        go test -race -count=1 -v -run "^TestPlatformLifecycleAcrossOwners$" ./internal/productplatform
    '
