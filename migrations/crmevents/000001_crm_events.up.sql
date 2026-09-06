-- CRM Events owns this database. External IDs are opaque references; no Core FKs.
CREATE TABLE event_sources (
    installation_id uuid PRIMARY KEY,
    integration_id uuid NOT NULL,
    initial_hours integer NOT NULL DEFAULT 48 CHECK (initial_hours BETWEEN 1 AND 168),
    retention_days integer NOT NULL DEFAULT 7 CHECK (retention_days BETWEEN 1 AND 90),
    continuous_from timestamptz,
    continuous_to timestamptz,
    retained_from timestamptz,
    last_success_at timestamptz,
    last_event_at timestamptz,
    state text NOT NULL DEFAULT 'pending',
    error_code text NOT NULL DEFAULT '',
    next_poll_at timestamptz NOT NULL DEFAULT now(),
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE event_consumers (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    consumer text NOT NULL CHECK (consumer = 'activity'),
    enabled boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, consumer)
);
CREATE TABLE event_operations (
    id uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    actor_id bigint NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    requested_from timestamptz,
    requested_to timestamptz,
    processed bigint NOT NULL DEFAULT 0,
    inserted bigint NOT NULL DEFAULT 0,
    updated bigint NOT NULL DEFAULT 0,
    deduplicated bigint NOT NULL DEFAULT 0,
    error_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE event_inbox (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    command_id text NOT NULL,
    payload_hash bytea NOT NULL,
    operation_id uuid NOT NULL REFERENCES event_operations(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, command_id)
);
CREATE TABLE event_jobs (
    id uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    operation_id uuid REFERENCES event_operations(id),
    kind text NOT NULL CHECK (kind IN ('current','backfill')),
    priority integer NOT NULL,
    window_from timestamptz NOT NULL,
    window_to timestamptz NOT NULL,
    target_to timestamptz NOT NULL,
    page integer NOT NULL DEFAULT 1,
    scan_pass integer NOT NULL DEFAULT 1,
    pass_digest text NOT NULL DEFAULT '',
    previous_digest text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'queued',
    attempts integer NOT NULL DEFAULT 0,
    lease_token bigint NOT NULL DEFAULT 0,
    run_after timestamptz NOT NULL DEFAULT now(),
    error_code text NOT NULL DEFAULT '',
    processed bigint NOT NULL DEFAULT 0,
    inserted bigint NOT NULL DEFAULT 0,
    updated bigint NOT NULL DEFAULT 0,
    deduplicated bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (window_from <= window_to AND window_to <= target_to)
);
CREATE UNIQUE INDEX event_jobs_one_current ON event_jobs(installation_id) WHERE kind = 'current' AND status IN ('queued','running','retry','paused');
CREATE INDEX event_jobs_claim ON event_jobs(priority, run_after, created_at) WHERE status IN ('queued','running','retry');
CREATE TABLE crm_events (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    event_id text NOT NULL,
    created_at timestamptz NOT NULL,
    created_by bigint NOT NULL,
    event_type text NOT NULL,
    entity_id bigint NOT NULL,
    entity_type text NOT NULL,
    value_before jsonb NOT NULL DEFAULT '[]',
    value_after jsonb NOT NULL DEFAULT '[]',
    content_hash bytea NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, event_id)
);
CREATE INDEX crm_events_time ON crm_events(installation_id, created_at, event_id);
CREATE INDEX crm_events_user_time ON crm_events(installation_id, created_by, created_at, event_id);
CREATE TABLE event_coverage (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    window_from timestamptz NOT NULL,
    window_to timestamptz NOT NULL,
    checked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, window_from, window_to)
);
-- Explicit commands may attach to an already pending current scan.
CREATE TABLE event_operation_jobs (
    operation_id uuid NOT NULL REFERENCES event_operations(id),
    job_id uuid NOT NULL REFERENCES event_jobs(id),
    PRIMARY KEY(operation_id,job_id)
);
