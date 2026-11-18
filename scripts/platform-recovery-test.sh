#!/bin/sh
# Only disposable, uniquely named resources are accepted; no external DSN is an input.
set -eu
: "${PLATFORM_RECOVERY_CURRENT_IMAGE:?set immutable current Platform sha256 image ID}"
: "${PLATFORM_RECOVERY_MIGRATE_IMAGE:?set immutable current migrator sha256 image ID}"
: "${PLATFORM_RECOVERY_ROLLBACK_IMAGE:?set immutable P09 Platform sha256 image ID}"
: "${PLATFORM_RECOVERY_TEST_IMAGE:?set immutable test-base sha256 image ID}"
for image in "$PLATFORM_RECOVERY_CURRENT_IMAGE" "$PLATFORM_RECOVERY_MIGRATE_IMAGE" "$PLATFORM_RECOVERY_ROLLBACK_IMAGE" "$PLATFORM_RECOVERY_TEST_IMAGE"; do
    printf '%s\n' "$image" | grep -Eq '^sha256:[0-9a-f]{64}$' || { echo 'Recovery requires immutable sha256 image IDs' >&2; exit 1; }
    test "$(docker image inspect --format '{{.Id}}' "$image")" = "$image"
done
test "$PLATFORM_RECOVERY_CURRENT_IMAGE" != "$PLATFORM_RECOVERY_ROLLBACK_IMAGE" || { echo 'Current and P09 rollback must be different images' >&2; exit 1; }
current_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$PLATFORM_RECOVERY_CURRENT_IMAGE")
rollback_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$PLATFORM_RECOVERY_ROLLBACK_IMAGE")
printf '%s\n' "$current_revision" | grep -Eq '^[0-9a-f]{40}$'
printf '%s\n' "$rollback_revision" | grep -Eq '^093cdb5[0-9a-f]{33}$'
test "$current_revision" != "$rollback_revision"
test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$PLATFORM_RECOVERY_MIGRATE_IMAGE")" = "$current_revision"
postgres_image=$(docker image inspect --format '{{.Id}}' "${PLATFORM_RECOVERY_POSTGRES_IMAGE:-postgres:17-alpine}")
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$root/tmp"
work=$(mktemp -d "$root/tmp/platform-recovery.XXXXXX")
name=$(basename "$work" | tr '[:upper:].' '[:lower:]-')
source_pg="$name-source"
target_pg="$name-target"
runner="$name-runner"
network="$name-network"
extract_current="$name-extract-current"
extract_rollback="$name-extract-rollback"
cleanup() {
    for container in "$runner" "$source_pg" "$target_pg" "$extract_current" "$extract_rollback"; do
        docker rm -f -v "$container" >/dev/null 2>&1 || :
    done
    docker network rm "$network" >/dev/null 2>&1 || :
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$work/proof" "$work/certs"
mkdir "$work/proof/zoneinfo"
# Extract the executable and time-zone data from each real release image.
docker create --name "$extract_current" "$PLATFORM_RECOVERY_CURRENT_IMAGE" >/dev/null
docker cp "$extract_current:/usr/local/bin/amocrm-platform" "$work/proof/current"
docker cp "$extract_current:/usr/share/zoneinfo" "$work/proof/zoneinfo/current"
test -s "$work/proof/zoneinfo/current/Europe/Moscow" || { echo 'Current image lacks Europe/Moscow time-zone data' >&2; exit 1; }
docker create --name "$extract_rollback" "$PLATFORM_RECOVERY_ROLLBACK_IMAGE" >/dev/null
docker cp "$extract_rollback:/usr/local/bin/amocrm-platform" "$work/proof/rollback"
docker cp "$extract_rollback:/usr/share/zoneinfo" "$work/proof/zoneinfo/rollback"
test -s "$work/proof/zoneinfo/rollback/Europe/Moscow" || { echo 'P09 image lacks Europe/Moscow time-zone data' >&2; exit 1; }
chmod +x "$work/proof/current" "$work/proof/rollback"
test "$(dd if="$work/proof/current" bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')" = 7f454c46
test "$(dd if="$work/proof/rollback" bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')" = 7f454c46
! cmp -s "$work/proof/current" "$work/proof/rollback"
docker network create "$network" >/dev/null
start_pg() {
    docker run -d --name "$1" --network "$network" -e POSTGRES_PASSWORD=recovery_only \
        -v "$root/deploy/platform/init.sql:/docker-entrypoint-initdb.d/10-platform.sql:ro" "$postgres_image" >/dev/null
    ready=0
    while test "$ready" -lt 60; do
        if docker exec "$1" pg_isready -h 127.0.0.1 -U postgres -d amocrm_platform >/dev/null 2>&1 && \
            docker exec "$1" psql -X -U postgres -d amocrm_platform -Atc 'SELECT 1' >/dev/null 2>&1; then return; fi
        ready=$((ready+1)); sleep 1
    done
    echo 'Disposable PostgreSQL did not become ready' >&2; exit 1
}
migrate() {
    docker run --rm --network "$network" -e "DATABASE_URL=postgres://platform_owner:platform_owner_dev@$1:5432/amocrm_platform?sslmode=disable" "$PLATFORM_RECOVERY_MIGRATE_IMAGE" up
}
ledger() {
    docker exec "$1" psql -X -U platform_owner -d amocrm_platform -Atc \
        "SELECT version,encode(checksum,'hex'),encode(down_checksum,'hex'),applied_at FROM schema_migrations ORDER BY version"
}
start_pg "$source_pg"
migrate "$source_pg"
docker run -d --name "$runner" --network "$network" -v "$root:/src:ro" -v "$work/proof:/proof" -v "$work/certs:/certificates" \
    -e ZONEINFO=/proof/zoneinfo/current -w /src --entrypoint sh "$PLATFORM_RECOVERY_TEST_IMAGE" -c 'while :; do sleep 60; done' >/dev/null
# Verify image labels against the actual ELF revision before accepting proof evidence.
test "$(docker exec "$runner" /proof/current --version)" = "$current_revision"
test "$(docker exec "$runner" /proof/rollback --version)" = "$rollback_revision"
test "$(docker run --rm "$PLATFORM_RECOVERY_MIGRATE_IMAGE" --version)" = "$current_revision"
# Compile once; every phase below runs this exact test executable.
docker exec "$runner" go test -race -c -o /proof/integration.test ./internal/productplatform
docker exec "$runner" go run ./cmd/service-certs --platform /certificates/generated
phase() {
    docker exec -e "PLATFORM_TEST_OWNER_DSN=postgres://platform_owner:platform_owner_dev@$1:5432/amocrm_platform?sslmode=disable" \
        -e "PLATFORM_TEST_ACCESS_DSN=postgres://platform_access:platform_access_dev@$1:5432/amocrm_platform?sslmode=disable" \
        -e "PLATFORM_TEST_JOBS_DSN=postgres://platform_jobs:platform_jobs_dev@$1:5432/amocrm_platform?sslmode=disable" \
        -e PLATFORM_TEST_CERTS_DIR=/certificates/generated -e "PLATFORM_TEST_BINARY=/proof/$2" -e "ZONEINFO=/proof/zoneinfo/$2" \
        -e "PLATFORM_RECOVERY_PHASE=$3" -e PLATFORM_RECOVERY_FACTS=/proof/facts.json \
        "$runner" /proof/integration.test -test.v -test.count=1 -test.run '^TestPlatformRecovery$'
}
phase "$source_pg" current seed
# The phase process exits only after all its app writers stop; dump a quiescent DB.
docker exec "$source_pg" pg_dump -U platform_owner -d amocrm_platform -Fc > "$work/proof/database.dump"
source_ledger=$(ledger "$source_pg")
start_pg "$target_pg"
# Roles/database/connect grants are recreated by init.sql; object ACLs and ownership come from the archive.
docker exec -i "$target_pg" pg_restore -U postgres -d amocrm_platform --exit-on-error < "$work/proof/database.dump"
test "$source_ledger" = "$(ledger "$target_pg")"
migrate "$target_pg"
test "$source_ledger" = "$(ledger "$target_pg")"
migrate "$target_pg"
test "$source_ledger" = "$(ledger "$target_pg")"
phase "$target_pg" current verify
phase "$target_pg" rollback verify
phase "$target_pg" current verify
# Failure coverage is destructive only to this now-completed disposable proof database.
docker exec -e "PLATFORM_TEST_OWNER_DSN=postgres://platform_owner:platform_owner_dev@$target_pg:5432/amocrm_platform?sslmode=disable" \
    -e "PLATFORM_TEST_ACCESS_DSN=postgres://platform_access:platform_access_dev@$target_pg:5432/amocrm_platform?sslmode=disable" \
    -e "PLATFORM_TEST_JOBS_DSN=postgres://platform_jobs:platform_jobs_dev@$target_pg:5432/amocrm_platform?sslmode=disable" \
    -e PLATFORM_TEST_CERTS_DIR=/certificates/generated -e PLATFORM_TEST_BINARY=/proof/current \
    "$runner" /proof/integration.test -test.v -test.count=1 -test.run '^TestPlatformGatewayFailures$'
printf 'Recovery proof passed: current=%s rollback=%s; restore, two no-op migrations, current/P09/current, Gateway failures\n' "$current_revision" "$rollback_revision"
