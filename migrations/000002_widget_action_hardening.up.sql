ALTER TABLE jobs
    ADD COLUMN actor_type TEXT,
    ADD COLUMN actor_id TEXT,
    ADD COLUMN resource_type TEXT,
    ADD COLUMN resource_id TEXT;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_actor_pair CHECK (
        (actor_type IS NULL AND actor_id IS NULL)
        OR (
            actor_type IS NOT NULL AND actor_id IS NOT NULL
            AND btrim(actor_type) <> '' AND length(actor_type) <= 100
            AND btrim(actor_id) <> '' AND length(actor_id) <= 200
        )
    ),
    ADD CONSTRAINT jobs_resource_pair CHECK (
        (resource_type IS NULL AND resource_id IS NULL)
        OR (
            resource_type IS NOT NULL AND resource_id IS NOT NULL
            AND btrim(resource_type) <> '' AND length(resource_type) <= 100
            AND btrim(resource_id) <> '' AND length(resource_id) <= 200
        )
    );

CREATE INDEX jobs_actor_lookup_idx
    ON jobs (installation_id, actor_type, actor_id, created_at DESC)
    WHERE actor_type IS NOT NULL;

UPDATE jobs
SET actor_type = 'widget_user', actor_id = payload->>'user_id'
WHERE type = 'widget.ping'
  AND actor_type IS NULL AND actor_id IS NULL
  AND jsonb_typeof(payload) = 'object'
  AND payload->>'user_id' ~ '^[1-9][0-9]*$';

ALTER TABLE idempotency_keys
    DROP CONSTRAINT idempotency_keys_job_id_fkey,
    ADD CONSTRAINT idempotency_keys_job_id_fkey
        FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE RESTRICT,
    ADD CONSTRAINT idempotency_keys_state_consistency CHECK (
        (
            status = 'processing'
            AND job_id IS NULL
            AND response_status IS NULL
            AND response_body IS NULL
        )
        OR (
            status = 'completed'
            AND job_id IS NOT NULL
            AND response_status IS NOT NULL
            AND response_status = 202
            AND response_body IS NOT NULL
            AND jsonb_typeof(response_body) = 'object'
        )
        OR (
            status = 'failed'
            AND job_id IS NULL
            AND response_status IS NOT NULL
            AND response_status BETWEEN 400 AND 599
            AND response_body IS NOT NULL
            AND jsonb_typeof(response_body) = 'object'
        )
    );

ALTER TABLE audit_log
    ADD COLUMN correlation_job_id UUID
        REFERENCES jobs(id) ON DELETE SET NULL,
    ADD CONSTRAINT audit_log_correlation_job_id_key UNIQUE (correlation_job_id);
