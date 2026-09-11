#!/bin/sh
# Placement overlay validation and a throwaway two-instance CRM Events restore.
# Never uses compose project amocrm-activity or runtime DBs amocrm_*.
# Live gRPC cutover is opt-in and off by default (TRANSFER_LIVE=1).
set -eu

TRANSFER_PROJECT=${TRANSFER_PROJECT:-amocrm-stage8-transfer-test}

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

repo=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
[ -f "$repo/docker-compose.activity.yml" ] || die "repository root not found"
[ -f "$repo/docker-compose.activity-transfer.yml" ] || die "transfer overlay missing"

case "$TRANSFER_PROJECT" in
  *-test) ;;
  *) die "refusing compose project without -test suffix: $TRANSFER_PROJECT" ;;
esac
case "$TRANSFER_PROJECT" in
  amocrm-activity|amocrm-pro-activity-test)
    die "refusing reserved compose project: $TRANSFER_PROJECT" ;;
esac

command -v docker >/dev/null 2>&1 || die "docker is required"
if command -v docker-compose >/dev/null 2>&1; then
  compose_cmd=docker-compose
elif docker compose version >/dev/null 2>&1; then
  compose_cmd="docker compose"
else
  die "docker-compose is required"
fi

printf '== compose config (compute-only overlay) ==\n'
$compose_cmd -p "$TRANSFER_PROJECT" \
  -f "$repo/docker-compose.activity.yml" \
  -f "$repo/docker-compose.activity-transfer.yml" \
  config >/tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'core-plane' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'activity-plane' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'events-plane' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'rpc-plane' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'core-postgres' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'activity-postgres' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'events-postgres' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'activity:9091' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'crm-events:9092' /tmp/${TRANSFER_PROJECT}-config.yml
grep -q 'worker:9090' /tmp/${TRANSFER_PROJECT}-config.yml
$compose_cmd -f "$repo/docker-compose.activity.yml" config | grep -q '^name: amocrm-activity$'
printf 'PASS: overlay config has isolated planes and static RPC host:port names\n'
printf 'PASS: docker-compose.activity.yml without overlay keeps name amocrm-activity\n'

printf '== compose config (--profile transfer-db) ==\n'
$compose_cmd -p "$TRANSFER_PROJECT" --profile transfer-db \
  -f "$repo/docker-compose.activity.yml" \
  -f "$repo/docker-compose.activity-transfer.yml" \
  config >/tmp/${TRANSFER_PROJECT}-config-db.yml
grep -q 'events-postgres-target' /tmp/${TRANSFER_PROJECT}-config-db.yml
grep -q 'activity-postgres-target' /tmp/${TRANSFER_PROJECT}-config-db.yml
printf 'PASS: transfer-db profile exposes second PostgreSQL aliases for owner DB move\n'

if [ "${TRANSFER_LIVE:-0}" = "1" ]; then
  die "TRANSFER_LIVE gRPC cutover is not enabled in this rehearsal; start writers only from the runbook after fencing"
fi

printf '== two-instance CRM Events dump/restore ==\n'
started=$(date +%s)
tmpdir=$(mktemp -d)
compose="$tmpdir/docker-compose.stage8-transfer-db-test.yml"
cat >"$compose" <<EOF
name: $TRANSFER_PROJECT
services:
  postgres-src:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: stage8_admin
      POSTGRES_PASSWORD: stage8_admin_dev
      POSTGRES_DB: postgres
    volumes:
      - $repo:/src:ro
      - dump:/dump
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U stage8_admin -d postgres"]
      interval: 1s
      timeout: 3s
      retries: 30
  postgres-dst:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: stage8_admin
      POSTGRES_PASSWORD: stage8_admin_dev
      POSTGRES_DB: postgres
    volumes:
      - $repo:/src:ro
      - dump:/dump
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U stage8_admin -d postgres"]
      interval: 1s
      timeout: 3s
      retries: 30
volumes:
  dump:
EOF

cat >"$tmpdir/seed.sh" <<'INNER'
set -eu
if [ -n "${POSTGRES_USER:-}" ]; then
  PGUSER=$POSTGRES_USER
  export PGUSER
fi
if [ -n "${POSTGRES_PASSWORD:-}" ]; then
  PGPASSWORD=$POSTGRES_PASSWORD
  export PGPASSWORD
fi
PGUSER=${PGUSER:-postgres}
export PGUSER
psql -d postgres -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
CREATE ROLE events_owner LOGIN PASSWORD 'events_owner_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE events_runtime LOGIN PASSWORD 'events_runtime_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE events_transfer_source_test OWNER events_owner;
REVOKE ALL ON DATABASE events_transfer_source_test FROM PUBLIC;
GRANT CONNECT ON DATABASE events_transfer_source_test TO events_runtime;
SQL
PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_source_test -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO events_runtime;
SQL
for f in /src/migrations/crmevents/[0-9][0-9][0-9][0-9][0-9][0-9]_*.up.sql; do
  PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_source_test -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done
PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_source_test -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
INSERT INTO event_sources(installation_id,integration_id,continuous_from,continuous_to,state)
VALUES('a1000000-0000-4000-8000-000000000001','b1000000-0000-4000-8000-000000000001',now()-interval '1 day',now(),'idle');
INSERT INTO event_consumers(installation_id,consumer,enabled)
VALUES('a1000000-0000-4000-8000-000000000001','activity',true);
INSERT INTO event_operations(id,installation_id,actor_id,kind,status,inserted)
VALUES('c1000000-0000-4000-8000-000000000001','a1000000-0000-4000-8000-000000000001',17,'sync','completed',1);
INSERT INTO event_inbox(installation_id,command_id,payload_hash,operation_id)
VALUES('a1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001',decode(repeat('01',32),'hex'),'c1000000-0000-4000-8000-000000000001');
INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,content_hash)
VALUES('a1000000-0000-4000-8000-000000000001','synthetic-transfer-event',now(),17,'lead_added',42,'lead',decode(repeat('02',32),'hex'));
INSERT INTO event_coverage(installation_id,window_from,window_to)
VALUES('a1000000-0000-4000-8000-000000000001',now()-interval '1 day',now());
SQL
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_dump --format=custom --file=/dump/events.dump events_transfer_source_test
INNER

cat >"$tmpdir/restore.sh" <<'INNER'
set -eu
if [ -n "${POSTGRES_USER:-}" ]; then
  PGUSER=$POSTGRES_USER
  export PGUSER
fi
if [ -n "${POSTGRES_PASSWORD:-}" ]; then
  PGPASSWORD=$POSTGRES_PASSWORD
  export PGPASSWORD
fi
PGUSER=${PGUSER:-postgres}
export PGUSER
psql -d postgres -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
CREATE ROLE events_owner LOGIN PASSWORD 'events_owner_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE events_runtime LOGIN PASSWORD 'events_runtime_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE events_transfer_restored_test OWNER events_owner;
REVOKE ALL ON DATABASE events_transfer_restored_test FROM PUBLIC;
GRANT CONNECT ON DATABASE events_transfer_restored_test TO events_runtime;
SQL
PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_restored_test -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO events_runtime;
SQL
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_restore --exit-on-error --dbname=events_transfer_restored_test /dump/events.dump
result=$(PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_restored_test -At -v ON_ERROR_STOP=1 -c "SELECT (SELECT count(*) FROM crm_events WHERE event_id='synthetic-transfer-event')=1 AND (SELECT count(*) FROM event_coverage)=1 AND (SELECT count(*) FROM event_consumers WHERE enabled)=1 AND EXISTS (SELECT 1 FROM event_inbox i JOIN event_operations o ON o.id=i.operation_id WHERE o.id::text=i.command_id AND o.status='completed')")
test "$result" = t
runtime=$(PGUSER=events_runtime PGPASSWORD=events_runtime_dev psql -d events_transfer_restored_test -At -v ON_ERROR_STOP=1 -c "SELECT count(*) FROM crm_events")
test "$runtime" = 1
if PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d events_transfer_restored_test -v ON_ERROR_STOP=1 -c "INSERT INTO event_inbox(installation_id,command_id,payload_hash,operation_id) VALUES('a1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001',decode(repeat('01',32),'hex'),'c1000000-0000-4000-8000-000000000001')" >/tmp/dup.log 2>&1; then
  exit 1
fi
printf 'PASS: CRM Events restored onto a second PostgreSQL instance; consumer, coverage and command identity preserved\n'
INNER

cleanup() {
  $compose_cmd -p "$TRANSFER_PROJECT" -f "$compose" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$tmpdir"
  rm -f /tmp/${TRANSFER_PROJECT}-config.yml /tmp/${TRANSFER_PROJECT}-config-db.yml
}
trap cleanup EXIT INT TERM

$compose_cmd -p "$TRANSFER_PROJECT" -f "$compose" down --volumes --remove-orphans >/dev/null 2>&1 || true
$compose_cmd -p "$TRANSFER_PROJECT" -f "$compose" up --detach --wait postgres-src postgres-dst

$compose_cmd -p "$TRANSFER_PROJECT" -f "$compose" exec -T postgres-src sh <"$tmpdir/seed.sh"
$compose_cmd -p "$TRANSFER_PROJECT" -f "$compose" exec -T postgres-dst sh <"$tmpdir/restore.sh"

elapsed=$(($(date +%s) - started))
printf 'PASS: target Core origin stays unchanged in this data-plane rehearsal (no Core dump moved)\n'
printf 'NOTE: live drain/cutover of Activity then CRM Events processes was not executed\n'
printf 'RTO_TRANSFER_DB_SECONDS=%s\n' "$elapsed"
printf 'PROJECT=%s\n' "$TRANSFER_PROJECT"
