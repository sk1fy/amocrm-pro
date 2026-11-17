CREATE TABLE platform_lifecycle_manifest (
    version INTEGER PRIMARY KEY CHECK (version = 1),
    digest TEXT NOT NULL
);

CREATE TABLE platform_lifecycle_integrations (
    integration_id UUID PRIMARY KEY REFERENCES integrations(id),
    product_code TEXT NOT NULL
);

CREATE TABLE platform_lifecycle_scope (
    integration_id UUID NOT NULL REFERENCES platform_lifecycle_integrations(integration_id),
    account_id BIGINT NOT NULL CHECK (account_id > 0),
    PRIMARY KEY (integration_id, account_id)
);

CREATE TABLE platform_lifecycle_cursors (
    installation_id UUID PRIMARY KEY REFERENCES installations(id),
    generation BIGINT NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
    sequence BIGINT NOT NULL CHECK (sequence BETWEEN generation AND 9007199254740991),
    authorization_sequence BIGINT NOT NULL CHECK (authorization_sequence BETWEEN 1 AND sequence),
    closed BOOLEAN NOT NULL
);

CREATE TABLE platform_lifecycle_ledger (
    event_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL REFERENCES platform_lifecycle_cursors(installation_id),
    sequence BIGINT NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
    evidence_id UUID NOT NULL UNIQUE,
    reason TEXT NOT NULL,
    body BYTEA NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 8192),
    UNIQUE (installation_id, sequence)
);

CREATE TABLE platform_lifecycle_reconciliation (
    installation_id UUID NOT NULL REFERENCES installations(id),
    reason TEXT NOT NULL,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (installation_id, reason)
);

CREATE TABLE platform_lifecycle_uninstalls (
    id UUID PRIMARY KEY,
    installation_id UUID NOT NULL REFERENCES installations(id),
    state TEXT NOT NULL CHECK (state IN ('pending', 'confirmed', 'superseded')),
    key_rotated BOOLEAN NOT NULL DEFAULT false,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    CHECK ((state = 'pending') = (finished_at IS NULL))
);
CREATE UNIQUE INDEX platform_lifecycle_uninstalls_current_idx
    ON platform_lifecycle_uninstalls(installation_id) WHERE NOT key_rotated;


CREATE TABLE platform_lifecycle_deliveries (
    event_id UUID PRIMARY KEY REFERENCES platform_lifecycle_ledger(event_id),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);


CREATE INDEX platform_lifecycle_processing_webhook_idx ON jobs(installation_id)
    WHERE type='webhook.reconcile' AND status='processing';
