ALTER TABLE platform_lifecycle.products
    ADD COLUMN display_name TEXT CHECK (char_length(display_name) BETWEEN 1 AND 200 AND btrim(display_name) <> ''),
    ADD COLUMN line TEXT CHECK (line IN ('FREE', 'TOP', 'PRO'));

ALTER TABLE platform_lifecycle.episodes
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991),
    ADD COLUMN last_lifecycle_event TEXT CHECK (last_lifecycle_event IN ('installed', 'reinstalled', 'reauthorized', 'uninstalled')),
    ADD COLUMN reviewed_by UUID,
    ADD COLUMN reviewed_at TIMESTAMPTZ,
    ADD CONSTRAINT episode_review_complete CHECK ((reviewed_by IS NULL) = (reviewed_at IS NULL));

UPDATE platform_lifecycle.episodes e
SET last_lifecycle_event = latest.kind
FROM (
    SELECT DISTINCT ON (h.episode_id) h.episode_id, h.kind
    FROM platform_lifecycle.history h
    JOIN platform_lifecycle.inbox i USING (event_id)
    WHERE h.kind <> 'authorization_changed'
    ORDER BY h.episode_id, i.sequence DESC
) latest
WHERE e.id = latest.episode_id;

CREATE FUNCTION platform_lifecycle.project_episode_transition() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    UPDATE platform_lifecycle.episodes
    SET version = version + CASE WHEN NEW.kind IN ('installed', 'reinstalled') THEN 0 ELSE 1 END,
        last_lifecycle_event = CASE WHEN NEW.kind = 'authorization_changed' THEN last_lifecycle_event ELSE NEW.kind END
    WHERE id = NEW.episode_id;
    RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION platform_lifecycle.project_episode_transition() FROM PUBLIC;
CREATE TRIGGER project_episode_transition AFTER INSERT ON platform_lifecycle.history
    FOR EACH ROW EXECUTE FUNCTION platform_lifecycle.project_episode_transition();

CREATE SCHEMA platform_manager;

CREATE VIEW platform_manager.approved_pairs AS
SELECT approved.integration_id, approved.product_id, approved.amo_account_id
FROM (
    SELECT i.id AS integration_id, i.product_id, s.amo_account_id,
           count(*) OVER (PARTITION BY i.product_id, s.amo_account_id) AS mappings
    FROM platform_lifecycle.scope s
    JOIN platform_lifecycle.integrations i ON i.id = s.integration_id
) approved
WHERE approved.mappings = 1;

CREATE VIEW platform_manager.installations AS
SELECT e.id, pair.account_id, a.amo_account_id, pair.product_id, product.code AS product_code,
       COALESCE(product.display_name, product.code) AS product_name, product.line,
       e.installation_id, e.generation, e.opened_at, e.closed_at, e.version, e.reviewed_by, e.reviewed_at,
       CASE WHEN e.generation = 1 THEN 'install' ELSE 'reinstall' END AS kind,
       e.last_lifecycle_event,
       CASE WHEN e.closed_at IS NOT NULL THEN 'removed' ELSE 'current' END AS lifecycle,
       CASE WHEN e.closed_at IS NOT NULL THEN 'disabled'
            WHEN e.generation = c.generation THEN c.effective_authorization
            ELSE 'unknown' END AS authorization,
       term.valid_until, term.source AS term_source, COALESCE(term.entitlement_version, 1) AS entitlement_version
FROM platform_lifecycle.episodes e
JOIN platform_lifecycle.account_products pair USING (installation_id)
JOIN platform_lifecycle.accounts a ON a.id = pair.account_id
JOIN platform_manager.approved_pairs approved
    ON approved.integration_id = pair.integration_id AND approved.product_id = pair.product_id
    AND approved.amo_account_id = a.amo_account_id
JOIN platform_lifecycle.products product ON product.id = pair.product_id
JOIN platform_lifecycle.cursors c USING (installation_id)
LEFT JOIN platform_lifecycle.terms term ON term.account_id = pair.account_id AND term.product_id = pair.product_id
WHERE e.last_lifecycle_event IS NOT NULL;

CREATE TABLE platform_manager.snapshots (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    grants_version BIGINT NOT NULL CHECK (grants_version BETWEEN 1 AND 9007199254740991),
    query_hash BYTEA NOT NULL CHECK (octet_length(query_hash) = 32),
    scope_hash BYTEA NOT NULL CHECK (octet_length(scope_hash) = 32),
    snapshot_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    total INTEGER NOT NULL CHECK (total BETWEEN 1 AND 10000),
    reviewed INTEGER NOT NULL CHECK (reviewed BETWEEN 0 AND total)
);
CREATE INDEX platform_manager_snapshot_expiry ON platform_manager.snapshots (expires_at, id);

CREATE TABLE platform_manager.snapshot_items (
    snapshot_id UUID NOT NULL REFERENCES platform_manager.snapshots (id) ON DELETE CASCADE,
    installed_at TIMESTAMPTZ NOT NULL,
    episode_id UUID NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY (snapshot_id, installed_at, episode_id)
);

CREATE TABLE platform_manager.commands (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    idempotency_key UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind = 'installation.review'),
    episode_id UUID NOT NULL REFERENCES platform_lifecycle.episodes (id),
    expected_version BIGINT NOT NULL CHECK (expected_version BETWEEN 1 AND 9007199254740991),
    body_hash BYTEA NOT NULL CHECK (octet_length(body_hash) = 32),
    status SMALLINT,
    response BYTEA,
    etag TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (subject_id, idempotency_key),
    CHECK ((status IS NULL AND response IS NULL AND etag IS NULL)
        OR (status IS NOT NULL AND status IN (200, 409) AND octet_length(response) > 0 AND response IS NOT NULL AND (etag IS NOT NULL) = (status = 200)))
);

CREATE FUNCTION platform_manager.require_command_outcome() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    IF EXISTS (SELECT FROM platform_manager.commands WHERE id = NEW.id AND status IS NULL) THEN
        RAISE EXCEPTION 'manager command requires a terminal outcome' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.require_command_outcome() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER require_command_outcome
    AFTER INSERT OR UPDATE ON platform_manager.commands DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION platform_manager.require_command_outcome();

CREATE TABLE platform_manager.review_events (
    id UUID PRIMARY KEY,
    command_id UUID NOT NULL UNIQUE REFERENCES platform_manager.commands (id),
    episode_id UUID NOT NULL REFERENCES platform_lifecycle.episodes (id),
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts (id),
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products (id),
    kind TEXT NOT NULL CHECK (kind IN ('installation_reviewed', 'installation_unreviewed')),
    actor_id UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    reason TEXT CHECK (char_length(reason) BETWEEN 1 AND 1000),
    version BIGINT NOT NULL CHECK (version BETWEEN 2 AND 9007199254740991),
    UNIQUE (episode_id, version)
);
CREATE TABLE platform_manager.event_outbox (
    event_id UUID PRIMARY KEY REFERENCES platform_manager.review_events (id),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);

GRANT UPDATE (version, last_lifecycle_event, reviewed_by, reviewed_at) ON platform_lifecycle.episodes TO platform_jobs;
GRANT USAGE ON SCHEMA platform_manager TO platform_jobs;
GRANT SELECT ON platform_manager.approved_pairs, platform_manager.installations TO platform_jobs;
GRANT SELECT, INSERT, DELETE ON platform_manager.snapshots TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.snapshot_items TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.commands TO platform_jobs;
GRANT UPDATE (status, response, etag) ON platform_manager.commands TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.review_events TO platform_jobs;
GRANT INSERT ON platform_manager.event_outbox TO platform_jobs;
