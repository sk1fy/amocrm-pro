CREATE TABLE webhook_event_tombstones (
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    deduplication_key BYTEA NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (installation_id, deduplication_key),
    CONSTRAINT webhook_event_tombstones_key_length
        CHECK (octet_length(deduplication_key) = 32),
    CONSTRAINT webhook_event_tombstones_seen_order
        CHECK (last_seen_at >= first_seen_at)
);

CREATE INDEX installations_active_account_domain_idx
    ON installations (lower(account_domain))
    WHERE status = 'active';

CREATE INDEX idempotency_keys_processing_expiry_idx
    ON idempotency_keys (expires_at)
    WHERE status = 'processing';

INSERT INTO webhook_event_tombstones (
    installation_id, deduplication_key, first_seen_at, last_seen_at
)
SELECT installation_id, deduplication_key, created_at, updated_at
FROM inbox_events
ON CONFLICT (installation_id, deduplication_key) DO NOTHING;

CREATE TABLE lead_status_workflow_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    source_pipeline_id BIGINT NOT NULL,
    source_status_id BIGINT NOT NULL,
    target_pipeline_id BIGINT NOT NULL,
    target_status_id BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT lead_status_workflow_rules_positive_ids CHECK (
        source_pipeline_id > 0 AND source_status_id > 0
        AND target_pipeline_id > 0 AND target_status_id > 0
    ),
    CONSTRAINT lead_status_workflow_rules_changes_state CHECK (
        source_pipeline_id <> target_pipeline_id
        OR source_status_id <> target_status_id
    ),
    CONSTRAINT lead_status_workflow_rules_id_installation_key UNIQUE (
        id, installation_id
    ),
    CONSTRAINT lead_status_workflow_rules_source_key UNIQUE (
        installation_id, source_pipeline_id, source_status_id
    )
);

CREATE INDEX lead_status_workflow_rules_enabled_idx
    ON lead_status_workflow_rules (
        installation_id, source_pipeline_id, source_status_id
    ) WHERE enabled;

CREATE TRIGGER lead_status_workflow_rules_set_updated_at
BEFORE UPDATE ON lead_status_workflow_rules
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

ALTER TABLE inbox_events
    ADD CONSTRAINT inbox_events_id_installation_key
        UNIQUE (id, installation_id);

ALTER TABLE jobs
    ADD CONSTRAINT jobs_id_installation_key
        UNIQUE (id, installation_id);

CREATE TABLE workflow_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    workflow_type TEXT NOT NULL,
    workflow_version INTEGER NOT NULL DEFAULT 1,
    origin_deduplication_key BYTEA NOT NULL,
    origin_event_id UUID,
    rule_id UUID,
    job_id UUID,
    status TEXT NOT NULL DEFAULT 'queued',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,

    CONSTRAINT workflow_runs_origin_key UNIQUE (
        installation_id, workflow_type, workflow_version,
        origin_deduplication_key
    ),
    CONSTRAINT workflow_runs_id_installation_key UNIQUE (id, installation_id),
    CONSTRAINT workflow_runs_job_key UNIQUE (job_id),
    CONSTRAINT workflow_runs_origin_event_installation_fkey
        FOREIGN KEY (origin_event_id, installation_id)
        REFERENCES inbox_events(id, installation_id)
        ON DELETE SET NULL (origin_event_id),
    CONSTRAINT workflow_runs_rule_installation_fkey
        FOREIGN KEY (rule_id, installation_id)
        REFERENCES lead_status_workflow_rules(id, installation_id) ON DELETE RESTRICT,
    CONSTRAINT workflow_runs_job_installation_fkey
        FOREIGN KEY (job_id, installation_id)
        REFERENCES jobs(id, installation_id)
        ON DELETE SET NULL (job_id),
    CONSTRAINT workflow_runs_type_not_blank CHECK (btrim(workflow_type) <> ''),
    CONSTRAINT workflow_runs_version_positive CHECK (workflow_version > 0),
    CONSTRAINT workflow_runs_origin_key_length
        CHECK (octet_length(origin_deduplication_key) = 32),
    CONSTRAINT workflow_runs_status_check CHECK (
        status IN ('queued', 'processing', 'completed', 'failed', 'dead')
    ),
    CONSTRAINT workflow_runs_finished_after_creation
        CHECK (finished_at IS NULL OR finished_at >= created_at)
);

CREATE INDEX workflow_runs_installation_created_idx
    ON workflow_runs (installation_id, created_at DESC);

CREATE TABLE outbound_effects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    workflow_run_id UUID,
    correlation_job_id UUID NOT NULL,
    effect_type TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    desired_state JSONB NOT NULL,
    desired_hash BYTEA NOT NULL,
    state TEXT NOT NULL DEFAULT 'prepared',
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at TIMESTAMPTZ,
    observed_at TIMESTAMPTZ,
    correlation_expires_at TIMESTAMPTZ NOT NULL,
    correlated_event_deduplication_key BYTEA,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbound_effects_correlation_job_key UNIQUE (correlation_job_id),
    CONSTRAINT outbound_effects_run_installation_fkey
        FOREIGN KEY (workflow_run_id, installation_id)
        REFERENCES workflow_runs(id, installation_id) ON DELETE RESTRICT,
    CONSTRAINT outbound_effects_job_installation_fkey
        FOREIGN KEY (correlation_job_id, installation_id)
        REFERENCES jobs(id, installation_id) ON DELETE RESTRICT,
    CONSTRAINT outbound_effects_type_not_blank CHECK (btrim(effect_type) <> ''),
    CONSTRAINT outbound_effects_resource_not_blank CHECK (
        btrim(resource_type) <> '' AND btrim(resource_id) <> ''
    ),
    CONSTRAINT outbound_effects_desired_state_object
        CHECK (jsonb_typeof(desired_state) = 'object'),
    CONSTRAINT outbound_effects_desired_hash_length
        CHECK (octet_length(desired_hash) = 32),
    CONSTRAINT outbound_effects_correlated_key_length CHECK (
        correlated_event_deduplication_key IS NULL
        OR octet_length(correlated_event_deduplication_key) = 32
    ),
    CONSTRAINT outbound_effects_state_check CHECK (
        state IN ('prepared', 'applied', 'uncertain', 'observed', 'no_effect', 'failed', 'expired')
    ),
    CONSTRAINT outbound_effects_correlation_expiry
        CHECK (correlation_expires_at > attempted_at),
    CONSTRAINT outbound_effects_applied_after_attempt CHECK (
        applied_at IS NULL OR applied_at >= attempted_at
    ),
    CONSTRAINT outbound_effects_observed_after_attempt CHECK (
        observed_at IS NULL OR observed_at >= attempted_at
    )
);

CREATE INDEX outbound_effects_active_correlation_idx
    ON outbound_effects (
        installation_id, effect_type, resource_type, resource_id, desired_hash,
        attempted_at DESC
    ) WHERE state IN ('prepared', 'applied', 'uncertain');

CREATE INDEX outbound_effects_expiry_idx
    ON outbound_effects (correlation_expires_at)
    WHERE state IN ('prepared', 'applied', 'uncertain');

CREATE TRIGGER outbound_effects_set_updated_at
BEFORE UPDATE ON outbound_effects
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

ALTER TABLE inbox_events
    ADD COLUMN correlated_effect_id UUID
        REFERENCES outbound_effects(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX inbox_events_correlated_effect_key
    ON inbox_events (correlated_effect_id)
    WHERE correlated_effect_id IS NOT NULL;
