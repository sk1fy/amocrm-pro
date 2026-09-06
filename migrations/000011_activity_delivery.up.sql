-- Core owns admission and delivery only. No CRM event or product-setting data.
CREATE TABLE activity_pilots (
    installation_id UUID PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT false,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE activity_command_receipts (
    command_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    integration_id UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    actor_id BIGINT NOT NULL CHECK (actor_id > 0),
    target TEXT NOT NULL CHECK (target IN ('activity', 'crm-events')),
    action TEXT NOT NULL CHECK (action IN ('settings', 'sync')),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version=1),
    key_hash BYTEA NOT NULL CHECK (octet_length(key_hash)=32),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(installation_id, target, action, key_hash)
);

CREATE TABLE activity_command_outbox (
    command_id UUID PRIMARY KEY REFERENCES activity_command_receipts(command_id) ON DELETE CASCADE,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload)='object'),
    status TEXT NOT NULL DEFAULT 'pending_delivery' CHECK (status IN ('pending_delivery','delivering','accepted','failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 20 CHECK (max_attempts BETWEEN 1 AND 100),
    run_after TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token UUID,
    leased_until TIMESTAMPTZ,
    error_code TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status='delivering')=(lease_token IS NOT NULL AND leased_until IS NOT NULL))
);
CREATE INDEX activity_delivery_ready ON activity_command_outbox (run_after, command_id) WHERE status IN ('pending_delivery','delivering');
CREATE INDEX activity_receipts_actor ON activity_command_receipts (installation_id, actor_id, created_at);
-- Receipts/inbox are intentionally retained indefinitely in v0. An outbox retry
-- horizon cannot outlive receiver dedup through an unrelated Core cleanup job.
