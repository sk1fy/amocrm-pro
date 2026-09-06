#!/bin/sh
# Only the isolated development/test Compose cluster may run this helper.
set -eu
for test_db in core_components_test activity_components_test events_components_test; do
  psql -v ON_ERROR_STOP=1 -v db="$test_db" --username "$POSTGRES_USER" --dbname postgres <<'SQL'
SELECT format('CREATE DATABASE %I', :'db') WHERE NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=:'db') \gexec
SQL
done
