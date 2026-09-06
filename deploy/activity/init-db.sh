#!/bin/sh
# Development-only fresh cluster bootstrap. Existing databases are never reset.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE core_owner LOGIN PASSWORD 'core_owner_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE activity_owner LOGIN PASSWORD 'activity_owner_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE events_owner LOGIN PASSWORD 'events_owner_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE core_runtime LOGIN PASSWORD 'core_runtime_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE activity_runtime LOGIN PASSWORD 'activity_runtime_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE events_runtime LOGIN PASSWORD 'events_runtime_dev' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE amocrm_core OWNER core_owner;
CREATE DATABASE amocrm_activity OWNER activity_owner;
CREATE DATABASE amocrm_events OWNER events_owner;
REVOKE ALL ON DATABASE amocrm_core, amocrm_activity, amocrm_events FROM PUBLIC;
GRANT CONNECT ON DATABASE amocrm_core TO core_runtime;
GRANT CONNECT ON DATABASE amocrm_activity TO activity_runtime;
GRANT CONNECT ON DATABASE amocrm_events TO events_runtime;
\connect amocrm_core
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO core_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE core_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO core_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE core_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO core_runtime;
\connect amocrm_activity
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO activity_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE activity_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO activity_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE activity_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO activity_runtime;
\connect amocrm_events
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO events_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE events_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO events_runtime;
SQL
