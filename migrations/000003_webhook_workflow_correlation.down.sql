DROP INDEX IF EXISTS inbox_events_correlated_effect_key;

ALTER TABLE inbox_events
    DROP COLUMN IF EXISTS correlated_effect_id;

DROP TABLE IF EXISTS outbound_effects;
DROP TABLE IF EXISTS workflow_runs;

ALTER TABLE jobs
    DROP CONSTRAINT IF EXISTS jobs_id_installation_key;

ALTER TABLE inbox_events
    DROP CONSTRAINT IF EXISTS inbox_events_id_installation_key;

DROP TRIGGER IF EXISTS lead_status_workflow_rules_set_updated_at
    ON lead_status_workflow_rules;
DROP TABLE IF EXISTS lead_status_workflow_rules;
DROP TABLE IF EXISTS webhook_event_tombstones;

DROP INDEX IF EXISTS idempotency_keys_processing_expiry_idx;
DROP INDEX IF EXISTS installations_active_account_domain_idx;
