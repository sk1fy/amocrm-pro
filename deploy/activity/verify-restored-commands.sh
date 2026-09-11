#!/bin/sh
# Read-only preflight for a restored, quiesced set of owner databases.
# Connection strings are supplied by the operator, never written to evidence.
set -eu
umask 077

: "${CORE_RESTORE_DSN:?Core restored database connection is required}"
: "${ACTIVITY_RESTORE_DSN:?Activity restored database connection is required}"
: "${EVENTS_RESTORE_DSN:?CRM Events restored database connection is required}"
command -v psql >/dev/null 2>&1 || { echo 'psql is required' >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM
export LC_ALL=C

read_owner() {
  # Suppress libpq errors, which can contain connection details. The caller
  # identifies the failed owner without echoing DSNs or SQL data.
  PGCONNECT_TIMEOUT=5 PGOPTIONS="${PGOPTIONS:-} -c default_transaction_read_only=on -c statement_timeout=60000" \
    psql -X -q -A -t -v ON_ERROR_STOP=1 --dbname="$1" 2>"$work/error"
}

if ! read_owner "$CORE_RESTORE_DSN" >"$work/core" <<'SQL'
SELECT r.target || '|' || r.integration_id || '|' || r.installation_id || '|' || r.actor_id || '|' || r.command_id
FROM activity_command_receipts r JOIN activity_command_outbox o USING(command_id)
WHERE o.status='accepted' AND r.created_at >= now()-interval '7 days';
SQL
then
  echo 'Core restore preflight query failed' >&2; exit 1
fi

if ! read_owner "$ACTIVITY_RESTORE_DSN" >"$work/activity" <<'SQL'
SELECT 'activity|' || integration_id || '|' || installation_id || '|' || actor_id || '|' || command_id
FROM command_receipts;
SQL
then
  echo 'Activity restore preflight query failed' >&2; exit 1
fi

if ! read_owner "$EVENTS_RESTORE_DSN" >"$work/events" <<'SQL'
SELECT 'crm-events|' || s.integration_id || '|' || i.installation_id || '|' || o.actor_id || '|' || i.command_id
FROM event_inbox i
JOIN event_sources s ON s.installation_id=i.installation_id
JOIN event_operations o ON o.id=i.operation_id AND o.installation_id=i.installation_id
WHERE o.id::text=i.command_id;
SQL
then
  echo 'CRM Events restore preflight query failed' >&2; exit 1
fi

sort -u "$work/core" >"$work/accepted"
sort -u "$work/activity" "$work/events" >"$work/received"
comm -23 "$work/accepted" "$work/received" >"$work/missing"
if [ -s "$work/missing" ]; then
  echo 'FAIL: accepted Core commands lack matching receiver identity; keep writers stopped and restore a coordinated backup set' >&2
  # Only a count; no tenant IDs, command payloads or credentials in the log.
  printf 'MISSING_RECEIVER_IDENTITIES=%s\n' "$(wc -l <"$work/missing" | tr -d ' ')" >&2
  exit 1
fi
echo 'PASS: accepted Core commands within the 7-day redelivery horizon have matching receiver identities'
echo 'NOTE: this preflight does not prove snapshot consistency; all owner writers must remain stopped'
