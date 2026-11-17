CREATE SCHEMA platform_lifecycle;

CREATE TABLE platform_lifecycle.manifest (
    version INTEGER PRIMARY KEY CHECK (version = 1),
    digest TEXT NOT NULL
);
CREATE TABLE platform_lifecycle.products (
    id UUID PRIMARY KEY,
    code TEXT NOT NULL UNIQUE
);
CREATE TABLE platform_lifecycle.integrations (
    id UUID PRIMARY KEY,
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id)
);
CREATE TABLE platform_lifecycle.scope (
    integration_id UUID NOT NULL REFERENCES platform_lifecycle.integrations(id),
    amo_account_id BIGINT NOT NULL CHECK (amo_account_id > 0),
    PRIMARY KEY (integration_id, amo_account_id)
);

CREATE TABLE platform_lifecycle.inbox (
    event_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
    body BYTEA NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 8192),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (installation_id, sequence)
);
CREATE TABLE platform_lifecycle.cursors (
    installation_id UUID PRIMARY KEY,
    sequence BIGINT NOT NULL DEFAULT 0,
    generation BIGINT NOT NULL DEFAULT 0,
    authorization_sequence BIGINT NOT NULL DEFAULT 0,
    effective_authorization TEXT NOT NULL DEFAULT 'unknown'
        CHECK (effective_authorization IN ('authorized', 'reauth_required', 'disabled', 'unknown')),
    last_lifecycle_event TEXT,
    CHECK (authorization_sequence BETWEEN 0 AND sequence),
    CHECK (generation BETWEEN 0 AND sequence)
);
CREATE TABLE platform_lifecycle.accounts (
    id UUID PRIMARY KEY,
    amo_account_id BIGINT NOT NULL UNIQUE CHECK (amo_account_id > 0)
);
CREATE TABLE platform_lifecycle.account_products (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id),
    installation_id UUID NOT NULL UNIQUE REFERENCES platform_lifecycle.cursors(installation_id),
    integration_id UUID NOT NULL REFERENCES platform_lifecycle.integrations(id),
    PRIMARY KEY (account_id, product_id)
);
CREATE TABLE platform_lifecycle.terms (
    account_id UUID NOT NULL,
    product_id UUID NOT NULL,
    valid_until TIMESTAMPTZ,
    source TEXT NOT NULL,
    reason TEXT,
    PRIMARY KEY (account_id, product_id),
    FOREIGN KEY (account_id, product_id) REFERENCES platform_lifecycle.account_products(account_id, product_id)
);
CREATE TABLE platform_lifecycle.episodes (
    id UUID PRIMARY KEY,
    installation_id UUID NOT NULL REFERENCES platform_lifecycle.account_products(installation_id),
    generation BIGINT NOT NULL CHECK (generation > 0),
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    UNIQUE (installation_id, generation)
);
CREATE UNIQUE INDEX platform_lifecycle_active_episode ON platform_lifecycle.episodes(installation_id) WHERE closed_at IS NULL;
CREATE TABLE platform_lifecycle.history (
    event_id UUID PRIMARY KEY REFERENCES platform_lifecycle.inbox(event_id),
    episode_id UUID NOT NULL REFERENCES platform_lifecycle.episodes(id),
    kind TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE platform_lifecycle.reconciliation (
    installation_id UUID NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    event_id UUID NOT NULL,
    reason TEXT NOT NULL,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (installation_id, sequence, event_id, reason)
);

GRANT USAGE ON SCHEMA platform_lifecycle TO platform_jobs;
GRANT SELECT ON platform_lifecycle.integrations, platform_lifecycle.scope TO platform_jobs;
GRANT SELECT, INSERT ON platform_lifecycle.inbox, platform_lifecycle.cursors, platform_lifecycle.accounts,
    platform_lifecycle.account_products, platform_lifecycle.episodes, platform_lifecycle.history,
    platform_lifecycle.reconciliation TO platform_jobs;
GRANT UPDATE (sequence, generation, authorization_sequence, effective_authorization, last_lifecycle_event)
    ON platform_lifecycle.cursors TO platform_jobs;
GRANT UPDATE (closed_at) ON platform_lifecycle.episodes TO platform_jobs;
GRANT UPDATE (resolved_at) ON platform_lifecycle.reconciliation TO platform_jobs;

CREATE FUNCTION platform_lifecycle.enqueue_drain(p_installation UUID, p_sequence BIGINT, p_event UUID)
RETURNS VOID LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    INSERT INTO platform_jobs.jobs(id,kind,payload)
    SELECT p_event, 'lifecycle.drain', jsonb_build_object('installation_id',p_installation)
    WHERE EXISTS(SELECT 1 FROM platform_lifecycle.reconciliation
                 WHERE installation_id=p_installation AND sequence=p_sequence AND event_id=p_event
                   AND reason='drain_pending' AND resolved_at IS NULL)
    ON CONFLICT DO NOTHING
$$;
REVOKE ALL ON FUNCTION platform_lifecycle.enqueue_drain(UUID, BIGINT, UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_lifecycle.enqueue_drain(UUID, BIGINT, UUID) TO platform_jobs;
