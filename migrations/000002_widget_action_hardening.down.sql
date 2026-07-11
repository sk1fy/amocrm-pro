ALTER TABLE audit_log
    DROP CONSTRAINT audit_log_correlation_job_id_key,
    DROP COLUMN correlation_job_id;

ALTER TABLE idempotency_keys
    DROP CONSTRAINT idempotency_keys_state_consistency,
    DROP CONSTRAINT idempotency_keys_job_id_fkey,
    ADD CONSTRAINT idempotency_keys_job_id_fkey
        FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE SET NULL;

DROP INDEX jobs_actor_lookup_idx;

ALTER TABLE jobs
    DROP CONSTRAINT jobs_resource_pair,
    DROP CONSTRAINT jobs_actor_pair,
    DROP COLUMN resource_id,
    DROP COLUMN resource_type,
    DROP COLUMN actor_id,
    DROP COLUMN actor_type;
