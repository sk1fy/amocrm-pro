#!/bin/sh
# Synthetic multi-owner backup/restore. Never points at runtime DBs
# amocrm_core / amocrm_activity / amocrm_events. Companion to
# deploy/activity/verify-backup.sh (CRM Events-only); do not replace that script.
#
# Inside an isolated postgres:17 container with the repo at /src:
#   sh /src/deploy/activity/verify-backup-owners.sh
#
# Host launcher (compose project amocrm-stage8-backup-test only):
#   sh deploy/activity/verify-backup-owners.sh --isolated
set -eu

SRC_DIR=${SRC_DIR:-/src}
FORBIDDEN_DBS='amocrm_core amocrm_activity amocrm_events'
BACKUP_PROJECT=amocrm-stage8-backup-test

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

assert_test_db() {
  case " $FORBIDDEN_DBS " in
    *" $1 "*) die "refusing runtime database name: $1" ;;
  esac
  case "$1" in
    *_test) ;;
    *) die "refusing database name without _test suffix: $1" ;;
  esac
}

compose_bin() {
  if command -v docker-compose >/dev/null 2>&1; then
    printf '%s\n' docker-compose
    return
  fi
  if docker compose version >/dev/null 2>&1; then
    printf '%s\n' "docker compose"
    return
  fi
  die "docker-compose is required for --isolated"
}

run_isolated() {
  repo=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
  command -v docker >/dev/null 2>&1 || die "docker is required for --isolated"
  compose_cmd=$(compose_bin)
  tmpdir=$(mktemp -d)
  compose="$tmpdir/docker-compose.stage8-backup-test.yml"
  cat >"$compose" <<EOF
name: $BACKUP_PROJECT
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: stage8_admin
      POSTGRES_PASSWORD: stage8_admin_dev
      POSTGRES_DB: postgres
    volumes:
      - $repo:/src:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U stage8_admin -d postgres"]
      interval: 1s
      timeout: 3s
      retries: 30
EOF
  cleanup_isolated() {
    $compose_cmd -p "$BACKUP_PROJECT" -f "$compose" down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$tmpdir"
  }
  trap cleanup_isolated EXIT INT TERM
  $compose_cmd -p "$BACKUP_PROJECT" -f "$compose" down --volumes --remove-orphans >/dev/null 2>&1 || true
  $compose_cmd -p "$BACKUP_PROJECT" -f "$compose" up --detach --wait postgres
  $compose_cmd -p "$BACKUP_PROJECT" -f "$compose" exec -T \
    -e SRC_DIR=/src \
    postgres sh /src/deploy/activity/verify-backup-owners.sh
}

if [ "${1:-}" = "--isolated" ]; then
  run_isolated
  exit 0
fi

[ -d "$SRC_DIR/migrations" ] || die "migrations not found under $SRC_DIR"
command -v psql >/dev/null 2>&1 || die "psql is required"
command -v pg_dump >/dev/null 2>&1 || die "pg_dump is required"
command -v pg_restore >/dev/null 2>&1 || die "pg_restore is required"
if [ -n "${POSTGRES_USER:-}" ]; then
  PGUSER=${PGUSER:-$POSTGRES_USER}
  export PGUSER
fi
if [ -n "${POSTGRES_PASSWORD:-}" ]; then
  PGPASSWORD=${PGPASSWORD:-$POSTGRES_PASSWORD}
  export PGPASSWORD
fi
PGUSER=${PGUSER:-postgres}
export PGUSER

started=$(date +%s)
suffix=$$
core_source="core_owners_${suffix}_source_test"
activity_source="activity_owners_${suffix}_source_test"
events_source="events_owners_${suffix}_source_test"
core_restored="core_owners_${suffix}_restored_test"
activity_restored="activity_owners_${suffix}_restored_test"
events_restored="events_owners_${suffix}_restored_test"
core_dump="/tmp/core-owner-${suffix}.dump"
activity_dump="/tmp/activity-owner-${suffix}.dump"
events_dump="/tmp/events-owner-${suffix}.dump"

for db in "$core_source" "$activity_source" "$events_source" "$core_restored" "$activity_restored" "$events_restored"; do
  assert_test_db "$db"
done

admin_psql() {
  psql -d postgres -v ON_ERROR_STOP=1 "$@"
}

if admin_psql -At -c "SELECT datname FROM pg_database WHERE datname IN ('amocrm_core','amocrm_activity','amocrm_events')" | grep -q .; then
  die "refusing cluster that already has runtime owner DBs amocrm_core/amocrm_activity/amocrm_events"
fi

ensure_role() {
  name=$1
  password=$2
  admin_psql -v name="$name" -v password="$password" <<'SQL' >/dev/null
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE', :'name', :'password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'name') \gexec
SQL
}

drop_db() {
  admin_psql -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" >/dev/null 2>&1
}

cleanup() {
  for db in "$core_source" "$activity_source" "$events_source" "$core_restored" "$activity_restored" "$events_restored"; do
    drop_db "$db" || true
  done
  rm -f "$core_dump" "$activity_dump" "$events_dump"
}
trap cleanup EXIT

ensure_role core_owner core_owner_dev
ensure_role activity_owner activity_owner_dev
ensure_role events_owner events_owner_dev
ensure_role core_runtime core_runtime_dev
ensure_role activity_runtime activity_runtime_dev
ensure_role events_runtime events_runtime_dev

create_owner_db() {
  db=$1
  owner=$2
  runtime=$3
  drop_db "$db"
  admin_psql -c "CREATE DATABASE $db OWNER $owner" >/dev/null
  admin_psql -c "REVOKE ALL ON DATABASE $db FROM PUBLIC" >/dev/null
  admin_psql -c "GRANT CONNECT ON DATABASE $db TO $runtime" >/dev/null
  admin_psql -c "GRANT CONNECT ON DATABASE $db TO $owner" >/dev/null
  PGUSER=$owner PGPASSWORD=${owner}_dev psql -d "$db" -v ON_ERROR_STOP=1 <<SQL >/dev/null
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO $runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE $owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO $runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE $owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO $runtime;
SQL
}

apply_sql_dir() {
  owner=$1
  db=$2
  dir=$3
  set -- "$dir"/[0-9][0-9][0-9][0-9][0-9][0-9]_*.up.sql
  [ -f "$1" ] || die "no up migrations in $dir"
  for f in "$dir"/[0-9][0-9][0-9][0-9][0-9][0-9]_*.up.sql; do
    PGUSER=$owner PGPASSWORD=${owner}_dev psql -d "$db" -v ON_ERROR_STOP=1 -f "$f" >/dev/null
  done
}

owner_psql() {
  owner=$1
  db=$2
  shift 2
  PGUSER=$owner PGPASSWORD=${owner}_dev psql -d "$db" -v ON_ERROR_STOP=1 "$@"
}

runtime_psql() {
  runtime=$1
  db=$2
  shift 2
  PGUSER=$runtime PGPASSWORD=${runtime}_dev psql -d "$db" -v ON_ERROR_STOP=1 "$@"
}

create_owner_db "$core_source" core_owner core_runtime
create_owner_db "$activity_source" activity_owner activity_runtime
create_owner_db "$events_source" events_owner events_runtime

apply_sql_dir core_owner "$core_source" "$SRC_DIR/migrations"
apply_sql_dir activity_owner "$activity_source" "$SRC_DIR/migrations/activity"
apply_sql_dir events_owner "$events_source" "$SRC_DIR/migrations/crmevents"

# Synthetic rows only. Ciphertext is dummy bytes, never selected into the log.
owner_psql core_owner "$core_source" <<'SQL' >/dev/null
INSERT INTO integrations (
  id, code, client_id, client_secret_ciphertext, client_secret_key_version, redirect_uri
) VALUES (
  '11111111-1111-4111-8111-111111111111',
  'synthetic-backup',
  'synthetic-client',
  decode(repeat('aa', 32), 'hex'),
  1,
  'https://example.invalid/oauth'
);
INSERT INTO installations (
  id, integration_id, account_id, account_domain, status
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  42,
  'synthetic.amocrm.ru',
  'active'
);
INSERT INTO oauth_credentials (
  installation_id, access_token_ciphertext, refresh_token_ciphertext, expires_at, key_version
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  decode(repeat('ab', 32), 'hex'),
  decode(repeat('cd', 32), 'hex'),
  now() + interval '1 hour',
  1
);
INSERT INTO integration_services (integration_id, service_code, enabled)
VALUES ('11111111-1111-4111-8111-111111111111', 'activity', true);
INSERT INTO activity_pilots (installation_id, enabled)
VALUES ('a1000000-0000-4000-8000-000000000001', true);
INSERT INTO activity_command_receipts (
  command_id, installation_id, integration_id, actor_id, target, action, key_hash, request_hash
) VALUES (
  'c1000000-0000-4000-8000-000000000001',
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  17,
  'crm-events',
  'sync',
  decode(repeat('11', 32), 'hex'),
  decode(repeat('22', 32), 'hex')
);
INSERT INTO activity_command_outbox (command_id, payload, status)
VALUES (
  'c1000000-0000-4000-8000-000000000001',
  '{"kind":"sync"}'::jsonb,
  'accepted'
);
-- A settings command has its own identity and receiver, not the Events ID.
INSERT INTO activity_command_receipts (
  command_id, installation_id, integration_id, actor_id, target, action, key_hash, request_hash
)
SELECT 'c2000000-0000-4000-8000-000000000002', installation_id, integration_id,
       actor_id, 'activity', 'settings', key_hash, request_hash
FROM activity_command_receipts WHERE command_id='c1000000-0000-4000-8000-000000000001';
INSERT INTO activity_command_outbox (command_id, payload, status)
VALUES ('c2000000-0000-4000-8000-000000000002', '{"initial_days":2,"retention_days":7}'::jsonb, 'accepted');
INSERT INTO installation_webhook_destinations (
  installation_id, destination_hash, destination_ciphertext, key_version
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  decode(repeat('ee', 32), 'hex'),
  decode(repeat('ff', 32), 'hex'),
  1
);
SQL

owner_psql activity_owner "$activity_source" <<'SQL' >/dev/null
INSERT INTO settings (installation_id, integration_id, initial_days, retention_days)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  2,
  7
);
INSERT INTO command_receipts (
  command_id, installation_id, integration_id, actor_id, request_hash
) VALUES (
  'c2000000-0000-4000-8000-000000000002',
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  17,
  decode(repeat('22', 32), 'hex')
);
SQL

owner_psql events_owner "$events_source" <<'SQL' >/dev/null
INSERT INTO event_sources (installation_id, integration_id, continuous_from, continuous_to, state)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  now() - interval '1 day',
  now(),
  'idle'
);
INSERT INTO event_consumers (installation_id, consumer, enabled)
VALUES ('a1000000-0000-4000-8000-000000000001', 'activity', true);
INSERT INTO event_operations (id, installation_id, actor_id, kind, status, inserted)
VALUES (
  'c1000000-0000-4000-8000-000000000001',
  'a1000000-0000-4000-8000-000000000001',
  17,
  'sync',
  'completed',
  1
);
INSERT INTO event_inbox (installation_id, command_id, payload_hash, operation_id)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'c1000000-0000-4000-8000-000000000001',
  decode(repeat('01', 32), 'hex'),
  'c1000000-0000-4000-8000-000000000001'
);
INSERT INTO event_jobs (
  id, installation_id, operation_id, kind, priority, window_from, window_to, target_to, status, inserted
) VALUES (
  'd1000000-0000-4000-8000-000000000001',
  'a1000000-0000-4000-8000-000000000001',
  'c1000000-0000-4000-8000-000000000001',
  'current',
  10,
  now() - interval '1 day',
  now(),
  now(),
  'completed',
  1
);
INSERT INTO crm_events (
  installation_id, event_id, created_at, created_by, event_type, entity_id, entity_type, content_hash, linked_talk_contact_id
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'synthetic-backup-event',
  now(),
  17,
  'lead_added',
  42,
  'lead',
  decode(repeat('02', 32), 'hex'),
  99
);
INSERT INTO event_coverage (installation_id, window_from, window_to)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  now() - interval '1 day',
  now()
);
INSERT INTO event_enrichment_objects (
  installation_id, object_kind, object_key, state, reason_code, source
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'note',
  'n:1',
  'ready',
  'ok',
  'synthetic'
);
INSERT INTO event_enrichment_links (
  installation_id, event_id, object_kind, object_key
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'synthetic-backup-event',
  'note',
  'n:1'
);
INSERT INTO event_command_tombstones (
  installation_id, command_id, payload_hash, command_created_at
) VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'expired-command',
  decode(repeat('03', 32), 'hex'),
  now() - interval '8 days'
);
SQL

PGUSER=core_owner PGPASSWORD=core_owner_dev pg_dump --format=custom --file="$core_dump" "$core_source"
PGUSER=activity_owner PGPASSWORD=activity_owner_dev pg_dump --format=custom --file="$activity_dump" "$activity_source"
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_dump --format=custom --file="$events_dump" "$events_source"

create_owner_db "$core_restored" core_owner core_runtime
create_owner_db "$activity_restored" activity_owner activity_runtime
create_owner_db "$events_restored" events_owner events_runtime

PGUSER=core_owner PGPASSWORD=core_owner_dev pg_restore --exit-on-error --dbname="$core_restored" "$core_dump"
PGUSER=activity_owner PGPASSWORD=activity_owner_dev pg_restore --exit-on-error --dbname="$activity_restored" "$activity_dump"
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_restore --exit-on-error --dbname="$events_restored" "$events_dump"

core_check=$(owner_psql core_owner "$core_restored" -At -c "
SELECT (
  (SELECT count(*) FROM activity_command_outbox WHERE command_id='c1000000-0000-4000-8000-000000000001' AND status='accepted')=1
  AND (SELECT count(*) FROM activity_command_receipts WHERE command_id='c1000000-0000-4000-8000-000000000001')=1
  AND (SELECT count(*) FROM oauth_credentials
       WHERE installation_id='a1000000-0000-4000-8000-000000000001'
         AND octet_length(access_token_ciphertext)>0
         AND octet_length(refresh_token_ciphertext)>0
         AND key_version=1)=1
  AND (SELECT count(*) FROM installation_webhook_destinations
       WHERE installation_id='a1000000-0000-4000-8000-000000000001'
         AND octet_length(destination_ciphertext)>0
         AND key_version=1)=1
  AND (SELECT count(*) FROM activity_pilots WHERE enabled)=1
  AND (SELECT count(*) FROM integration_services WHERE service_code='activity' AND enabled)=1
)")
test "$core_check" = t || die "core restore checks failed"

activity_check=$(owner_psql activity_owner "$activity_restored" -At -c "
SELECT (
  (SELECT count(*) FROM settings
     WHERE installation_id='a1000000-0000-4000-8000-000000000001'
       AND initial_days=2 AND retention_days=7)=1
  AND (SELECT count(*) FROM command_receipts
     WHERE command_id='c2000000-0000-4000-8000-000000000002')=1
)")
test "$activity_check" = t || die "activity restore checks failed"

events_check=$(owner_psql events_owner "$events_restored" -At -c "
SELECT (
  (SELECT count(*) FROM crm_events WHERE event_id='synthetic-backup-event' AND linked_talk_contact_id=99)=1
  AND (SELECT count(*) FROM event_coverage)=1
  AND (SELECT count(*) FROM event_inbox)=1
  AND (SELECT count(*) FROM event_jobs)=1
  AND (SELECT count(*) FROM event_consumers WHERE enabled)=1
  AND EXISTS (SELECT 1 FROM event_sources WHERE continuous_to>continuous_from)
  AND EXISTS (
    SELECT 1 FROM event_inbox i
    JOIN event_operations o ON o.id=i.operation_id
    WHERE o.id::text=i.command_id AND o.status='completed' AND o.inserted=1
  )
  AND (SELECT count(*) FROM event_enrichment_objects WHERE object_key='n:1')=1
  AND (SELECT count(*) FROM event_enrichment_links)=1
  AND (SELECT count(*) FROM event_command_tombstones WHERE command_id='expired-command')=1
)")
test "$events_check" = t || die "events restore checks failed"

# No writers run during these dumps/restores. Exercise the preflight against
# that coordinated set, then simulate a missing receiver in each owner.
restore_preflight() {
  CORE_RESTORE_DSN="dbname=$core_restored user=core_runtime password=core_runtime_dev" \
  ACTIVITY_RESTORE_DSN="dbname=$activity_restored user=activity_runtime password=activity_runtime_dev" \
  EVENTS_RESTORE_DSN="dbname=$events_restored user=events_runtime password=events_runtime_dev" \
    sh "$SRC_DIR/deploy/activity/verify-restored-commands.sh"
}
expect_skew_rejected() {
  if restore_preflight >"/tmp/restore-preflight-${suffix}.log" 2>&1; then
    die "restore preflight accepted missing or mismatched receiver identity"
  fi
  grep -q '^MISSING_RECEIVER_IDENTITIES=1$' "/tmp/restore-preflight-${suffix}.log" || die "preflight failed for an unexpected reason"
  rm -f "/tmp/restore-preflight-${suffix}.log"
}
restore_preflight
owner_psql events_owner "$events_restored" -c "DELETE FROM event_inbox" >/dev/null
expect_skew_rejected
# Pending delivery is allowed: the ordinary worker can still deliver it.
owner_psql core_owner "$core_restored" -c "UPDATE activity_command_outbox SET status='pending_delivery' WHERE command_id='c1000000-0000-4000-8000-000000000001'" >/dev/null
restore_preflight
owner_psql core_owner "$core_restored" -c "UPDATE activity_command_outbox SET status='accepted' WHERE command_id='c1000000-0000-4000-8000-000000000001'" >/dev/null
# Old receiver history may already have been collected; preflight must not
# treat it as a recent accepted command or extend its replay horizon.
owner_psql core_owner "$core_restored" -c "UPDATE activity_command_receipts SET created_at=now()-interval '8 days' WHERE command_id='c1000000-0000-4000-8000-000000000001'" >/dev/null
restore_preflight
owner_psql core_owner "$core_restored" -c "UPDATE activity_command_receipts SET created_at=now() WHERE command_id='c1000000-0000-4000-8000-000000000001'" >/dev/null
owner_psql events_owner "$events_restored" -c "INSERT INTO event_inbox(installation_id,command_id,payload_hash,operation_id) VALUES('a1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001',decode(repeat('01',32),'hex'),'c1000000-0000-4000-8000-000000000001')" >/dev/null
owner_psql activity_owner "$activity_restored" -c "DELETE FROM command_receipts" >/dev/null
expect_skew_rejected
owner_psql activity_owner "$activity_restored" -c "INSERT INTO command_receipts(command_id,installation_id,integration_id,actor_id,request_hash) VALUES('c2000000-0000-4000-8000-000000000002','a1000000-0000-4000-8000-000000000001','11111111-1111-4111-8111-111111111111',17,decode(repeat('22',32),'hex'))" >/dev/null
owner_psql events_owner "$events_restored" -c "UPDATE event_operations SET actor_id=18" >/dev/null
expect_skew_rejected
owner_psql events_owner "$events_restored" -c "UPDATE event_operations SET actor_id=17" >/dev/null
restore_preflight
if (events_restored=postgres; restore_preflight) >"/tmp/restore-preflight-${suffix}.log" 2>&1; then
  die "restore preflight accepted an unreadable Events owner"
fi
grep -q '^CRM Events restore preflight query failed$' "/tmp/restore-preflight-${suffix}.log" || die "unexpected unreadable-owner failure"
rm -f "/tmp/restore-preflight-${suffix}.log"
printf 'PASS: restore preflight rejects missing Events/Activity receipts, wrong actor and unreadable owner; pending delivery and expired history remain valid\n'

# Repeated command identity must keep dedup after restore.
if owner_psql events_owner "$events_restored" -c "
INSERT INTO event_inbox (installation_id, command_id, payload_hash, operation_id)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'c1000000-0000-4000-8000-000000000001',
  decode(repeat('01', 32), 'hex'),
  'c1000000-0000-4000-8000-000000000001'
)" >/tmp/events-dup-${suffix}.log 2>&1; then
  die "restored event_inbox accepted a duplicate command_id"
fi
if owner_psql activity_owner "$activity_restored" -c "
INSERT INTO command_receipts (command_id, installation_id, integration_id, actor_id, request_hash)
VALUES (
  'c2000000-0000-4000-8000-000000000002',
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  17,
  decode(repeat('22', 32), 'hex')
)" >/tmp/activity-dup-${suffix}.log 2>&1; then
  die "restored activity receipts accepted a duplicate command_id"
fi
if owner_psql core_owner "$core_restored" -c "
INSERT INTO activity_command_receipts (
  command_id, installation_id, integration_id, actor_id, target, action, key_hash, request_hash
) VALUES (
  'c1000000-0000-4000-8000-000000000001',
  'a1000000-0000-4000-8000-000000000001',
  '11111111-1111-4111-8111-111111111111',
  17,
  'crm-events',
  'sync',
  decode(repeat('11', 32), 'hex'),
  decode(repeat('22', 32), 'hex')
)" >/tmp/core-dup-${suffix}.log 2>&1; then
  die "restored core receipts accepted a duplicate command_id"
fi
rm -f /tmp/events-dup-${suffix}.log /tmp/activity-dup-${suffix}.log /tmp/core-dup-${suffix}.log

# Tombstone still rejects a late Apply of a collected command.
if owner_psql events_owner "$events_restored" -c "
INSERT INTO event_inbox (installation_id, command_id, payload_hash, operation_id)
VALUES (
  'a1000000-0000-4000-8000-000000000001',
  'expired-command',
  decode(repeat('03', 32), 'hex'),
  'c1000000-0000-4000-8000-000000000001'
)" >/tmp/events-tombstone-${suffix}.log 2>&1; then
  die "restored tombstone did not reject expired command replay"
fi
rm -f /tmp/events-tombstone-${suffix}.log

runtime_events=$(runtime_psql events_runtime "$events_restored" -At -c "SELECT count(*) FROM crm_events")
runtime_activity=$(runtime_psql activity_runtime "$activity_restored" -At -c "SELECT count(*) FROM settings")
runtime_core=$(runtime_psql core_runtime "$core_restored" -At -c "SELECT count(*) FROM activity_command_outbox")
test "$runtime_events" = 1 || die "events_runtime cannot read restored events"
test "$runtime_activity" = 1 || die "activity_runtime cannot read restored settings"
test "$runtime_core" = 2 || die "core_runtime cannot read restored outbox"

if runtime_psql events_runtime "$events_restored" -c "CREATE TABLE forbidden_ddl(id int)" >/tmp/events-ddl-${suffix}.log 2>&1; then
  die "events_runtime was allowed DDL on restored DB"
fi
rm -f /tmp/events-ddl-${suffix}.log

if PGUSER=core_runtime PGPASSWORD=core_runtime_dev psql -d "$events_restored" -c "SELECT 1" >/tmp/core-foreign-${suffix}.log 2>&1; then
  die "core_runtime connected to restored events DB"
fi
if PGUSER=activity_runtime PGPASSWORD=activity_runtime_dev psql -d "$core_restored" -c "SELECT 1" >/tmp/activity-foreign-${suffix}.log 2>&1; then
  die "activity_runtime connected to restored core DB"
fi
if PGUSER=events_runtime PGPASSWORD=events_runtime_dev psql -d "$activity_restored" -c "SELECT 1" >/tmp/events-foreign-${suffix}.log 2>&1; then
  die "events_runtime connected to restored activity DB"
fi
rm -f /tmp/core-foreign-${suffix}.log /tmp/activity-foreign-${suffix}.log /tmp/events-foreign-${suffix}.log

elapsed=$(($(date +%s) - started))
printf 'PASS: Core owner dump/restore preserved outbox, receipts, pilots, grants and credential ciphertext presence (values not printed)\n'
printf 'PASS: Activity owner dump/restore preserved settings and command receipts\n'
printf 'PASS: CRM Events owner dump/restore preserved events, coverage, source progress, consumer, jobs, enrichment, tombstones and command/operation identity\n'
printf 'PASS: repeated command IDs stay unique after restore; expired tombstone still rejects replay\n'
printf 'PASS: runtime roles keep DML on own restored DB, no DDL, no CONNECT to foreign owner DBs\n'
printf 'NOTE: these three dumps were taken with no writers; independent live snapshots are not a supported restore set\n'
printf 'RESTORED: %s %s %s\n' "$core_restored" "$activity_restored" "$events_restored"
printf 'RTO_LOCAL_SECONDS=%s\n' "$elapsed"
