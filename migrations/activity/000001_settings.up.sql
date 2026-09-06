CREATE TABLE settings (
    installation_id UUID PRIMARY KEY,
    integration_id UUID NOT NULL,
    initial_days INTEGER NOT NULL CHECK (initial_days BETWEEN 1 AND 7),
    retention_days INTEGER NOT NULL CHECK (retention_days BETWEEN 2 AND 30 AND retention_days >= initial_days),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE command_receipts (
    command_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    integration_id UUID NOT NULL,
    actor_id BIGINT NOT NULL CHECK(actor_id > 0),
    request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Retain receiver dedup for at least the Core delivery/retry horizon. v0 does
-- not delete receipts automatically; event retention is owned by CRM Events.
