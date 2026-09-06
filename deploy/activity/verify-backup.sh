#!/bin/sh
# Synthetic owner-only backup/restore verification; never points at runtime DBs.
# Run in postgres:17-alpine on the isolated pilot network with /src mounted.
set -eu
test_source="activity_backup_$$_source_test"
test_restored="activity_backup_$$_restored_test"
backup_file="/tmp/activity-owner-$$.dump"
cleanup() {
  psql -d postgres -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $test_source" >/dev/null
  psql -d postgres -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $test_restored" >/dev/null
  rm -f "$backup_file"
}
trap cleanup EXIT
psql -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE $test_source OWNER events_owner" >/dev/null
psql -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE $test_restored OWNER events_owner" >/dev/null
PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d "$test_source" -v ON_ERROR_STOP=1 -f /src/migrations/crmevents/000001_crm_events.up.sql >/dev/null
PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d "$test_source" -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
INSERT INTO event_sources(installation_id,integration_id,continuous_from,continuous_to,state)
VALUES('a1000000-0000-4000-8000-000000000001','b1000000-0000-4000-8000-000000000001',now()-interval '1 day',now(),'idle');
INSERT INTO event_consumers(installation_id,consumer,enabled) VALUES('a1000000-0000-4000-8000-000000000001','activity',true);
INSERT INTO event_operations(id,installation_id,actor_id,kind,status,inserted)
VALUES('c1000000-0000-4000-8000-000000000001','a1000000-0000-4000-8000-000000000001',17,'sync','completed',1);
INSERT INTO event_inbox(installation_id,command_id,payload_hash,operation_id)
VALUES('a1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001',decode(repeat('01',32),'hex'),'c1000000-0000-4000-8000-000000000001');
INSERT INTO event_jobs(id,installation_id,operation_id,kind,priority,window_from,window_to,target_to,status,inserted)
VALUES('d1000000-0000-4000-8000-000000000001','a1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001','current',10,now()-interval '1 day',now(),now(),'completed',1);
INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,content_hash)
VALUES('a1000000-0000-4000-8000-000000000001','synthetic-backup-event',now(),17,'lead_added',42,'lead',decode(repeat('02',32),'hex'));
INSERT INTO event_coverage(installation_id,window_from,window_to)
VALUES('a1000000-0000-4000-8000-000000000001',now()-interval '1 day',now());
SQL
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_dump --format=custom --file="$backup_file" "$test_source"
PGUSER=events_owner PGPASSWORD=events_owner_dev pg_restore --exit-on-error --dbname="$test_restored" "$backup_file"
check_sql="SELECT (SELECT count(*) FROM crm_events)=1 AND (SELECT count(*) FROM event_inbox)=1 AND (SELECT count(*) FROM event_jobs)=1 AND (SELECT count(*) FROM event_consumers WHERE enabled)=1 AND (SELECT count(*) FROM event_coverage)=1 AND EXISTS(SELECT 1 FROM event_sources WHERE continuous_to>continuous_from) AND EXISTS(SELECT 1 FROM event_inbox i JOIN event_operations o ON o.id=i.operation_id WHERE o.id::text=i.command_id AND o.status='completed' AND o.inserted=1)"
result=$(PGUSER=events_owner PGPASSWORD=events_owner_dev psql -d "$test_restored" -At -v ON_ERROR_STOP=1 -c "$check_sql")
test "$result" = t
printf 'PASS: synthetic CRM Events owner pg_dump/pg_restore preserves events, coverage, source progress, consumer, jobs and command/operation dedup identity\n'
