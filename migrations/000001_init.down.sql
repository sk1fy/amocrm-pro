DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS used_widget_tokens;
DROP TABLE IF EXISTS job_attempts;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS inbox_events;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS oauth_credentials;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS installations;
DROP TABLE IF EXISTS integrations;

DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS pgcrypto;
