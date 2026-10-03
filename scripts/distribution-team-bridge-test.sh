#!/bin/sh
# Actual private Core/TeamOS boundary acceptance; both databases are disposable.
set -eu
core_repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
team_repo=${TEAMOS_BACKEND_DIR:-"$core_repo/../team-os-backend"}
test -f "$team_repo/services/company/go.mod"
mkdir -p "$core_repo/tmp"
bridge_dir=$(mktemp -d "$core_repo/tmp/distribution-bridge.XXXXXX")
project="amocrm-pro-distribution-bridge-$$"
core_name="$project-core"
team_name="$project-team"
compose() {
 if command -v docker-compose >/dev/null 2>&1; then
  docker-compose -p "$project" -f "$core_repo/docker-compose.test.yml" "$@"
 else
  docker compose -p "$project" -f "$core_repo/docker-compose.test.yml" "$@"
 fi
}
cleanup() {
 original_status=$?
 trap - EXIT INT TERM
 touch "$bridge_dir/backup-stop"
 if [ -n "${backup_pid:-}" ]; then wait "$backup_pid" || true; fi
 docker stop "$team_name" >/dev/null 2>&1 || true
 docker rm "$team_name" >/dev/null 2>&1 || true
 docker stop "$core_name" >/dev/null 2>&1 || true
 docker rm "$core_name" >/dev/null 2>&1 || true
 compose down --volumes --remove-orphans >/dev/null 2>&1 || true
 printf 'Bridge evidence: %s\n' "$bridge_dir"
 exit "$original_status"
}
trap cleanup EXIT INT TERM
compose up --detach --wait postgres
compose run --rm migrate up
set -- "$core_repo"/migrations/*.up.sql
migration_count=$#
applied_count=$(compose exec -T postgres psql -U amocrm_test -d amocrm_test -Atc 'SELECT count(*) FROM schema_migrations')
if [ "$applied_count" != "$migration_count" ]; then
 printf 'Matching test images are required; run make integration-test before this bridge profile.\n' >&2
 exit 1
fi
pg_id=$(compose ps -q postgres)
pg_ip=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$pg_id")
docker run --name "$core_name" --network host --entrypoint go \
 -v "$core_repo:/src" -v "$bridge_dir:/bridge" -w /src \
 -e DISTRIBUTION_BRIDGE_DIR=/bridge \
 -e TEST_DATABASE_URL="postgres://amocrm_test:amocrm_test@$pg_ip:5432/amocrm_test?sslmode=disable" \
 -e TEST_DATABASE_RESET_ALLOWED=true \
 amocrm-integration-test:local test -race -count=1 -timeout=12m -v \
 -run '^TestDistributionTeamBridgeServer$' ./internal/distribution > "$bridge_dir/core.log" 2>&1 &
core_pid=$!
for step in $(seq 1 120); do
 if [ -f "$bridge_dir/core.json" ]; then break; fi
 if ! kill -0 "$core_pid" 2>/dev/null; then wait "$core_pid"; exit 1; fi
 sleep 1
done
test -f "$bridge_dir/core.json"
python3 "$core_repo/scripts/distribution-backup-fixture.py" "$bridge_dir" "$pg_id" "$project" > "$bridge_dir/backup.log" 2>&1 &
backup_pid=$!
# VM host networking gives both test processes the same loopback namespace.
# The socket mount is interpreted by the Docker daemon (also on Colima).
docker run --rm --name "$team_name" --network host --entrypoint go \
 -v "$team_repo:/src" -v "$bridge_dir:/bridge" \
 -v "${TEAMOS_GO_MOD_CACHE:-amocrm-go-mod}:/go/pkg/mod" \
 -v /var/run/docker.sock:/var/run/docker.sock -w /src/services/company \
 -e GOWORK=off -e DISTRIBUTION_BRIDGE_RUN_ID="$project" -e DOCKER_HOST=unix:///var/run/docker.sock \
 -e TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
 -e TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1 \
 -e DISTRIBUTION_BRIDGE_DIR=/bridge -e SSL_CERT_FILE=/bridge/core-ca.pem \
 amocrm-integration-test:local test -race -tags integration -count=1 -timeout=10m -v \
 -run '^TestRS06ActualCoreTeamSignedBridge$' ./internal/transport/distributionhttp > "$bridge_dir/team.log" 2>&1 &
team_pid=$!
while kill -0 "$team_pid" 2>/dev/null; do
 if ! kill -0 "$core_pid" 2>/dev/null && [ ! -f "$bridge_dir/core-stopped" ]; then
  docker stop "$team_name" >/dev/null 2>&1 || true
  wait "$team_pid" || true
  wait "$core_pid" || true
  exit 1
 fi
 sleep 1
done
wait "$team_pid"
wait "$core_pid"
