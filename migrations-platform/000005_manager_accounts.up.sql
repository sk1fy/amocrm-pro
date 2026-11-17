ALTER TABLE platform_jobs.jobs
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    ADD COLUMN failure_code TEXT CHECK (failure_code ~ '^[a-z_]{1,40}$');
GRANT UPDATE (updated_at, failure_code) ON platform_jobs.jobs TO platform_jobs;

ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_kind_check,
    DROP CONSTRAINT commands_check,
    ALTER COLUMN episode_id DROP NOT NULL,
    ALTER COLUMN expected_version DROP NOT NULL,
    ADD COLUMN account_id UUID REFERENCES platform_lifecycle.accounts(id),
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind = 'installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL AND account_id IS NULL)
        OR (kind = 'account.refresh' AND account_id IS NOT NULL AND episode_id IS NULL AND expected_version IS NULL)),
    ADD CONSTRAINT commands_outcome_shape CHECK (
        (status IS NULL AND response IS NULL AND etag IS NULL)
        OR (status IS NOT NULL AND response IS NOT NULL AND octet_length(response) > 0 AND (
            (kind = 'installation.review' AND status IN (200, 409) AND (etag IS NOT NULL) = (status = 200))
            OR (kind = 'account.refresh' AND status = 202 AND etag IS NULL)
        )));

CREATE VIEW platform_manager.account_memberships AS
SELECT pair.account_id, a.amo_account_id, pair.product_id, pair.installation_id, pair.integration_id,
       live.generation, live.authorization_sequence, live.effective_authorization,
       ep.id IS NOT NULL AS current
FROM platform_lifecycle.account_products pair
JOIN platform_lifecycle.accounts a ON a.id = pair.account_id
JOIN platform_manager.approved_pairs approved ON approved.integration_id = pair.integration_id
    AND approved.product_id = pair.product_id AND approved.amo_account_id = a.amo_account_id
JOIN platform_lifecycle.cursors live ON live.installation_id = pair.installation_id
LEFT JOIN platform_lifecycle.episodes ep ON ep.installation_id = pair.installation_id
    AND ep.generation = live.generation AND ep.closed_at IS NULL;

CREATE VIEW platform_manager.visible_accounts AS
SELECT account_id, amo_account_id, count(*) FILTER (WHERE current)::integer AS active_products_count
FROM platform_manager.account_memberships GROUP BY account_id, amo_account_id;

CREATE TABLE platform_manager.account_identities (
    account_id UUID PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    current_domain TEXT NOT NULL CHECK (char_length(current_domain) BETWEEN 1 AND 253),
    display_name TEXT NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 512),
    time_zone TEXT CHECK (char_length(time_zone) BETWEEN 1 AND 128),
    observed_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE platform_manager.account_domains (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    domain TEXT NOT NULL CHECK (char_length(domain) BETWEEN 1 AND 253),
    PRIMARY KEY (account_id, domain)
);
CREATE TABLE platform_manager.account_rosters (
    account_id UUID PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    roster JSONB,
    active_count INTEGER CHECK (active_count BETWEEN 0 AND 1000),
    evidence_id UUID,
    observed_at TIMESTAMPTZ,
    fresh_until TIMESTAMPTZ,
    failure_code TEXT CHECK (failure_code ~ '^[a-z_]{1,40}$'),
    CHECK (num_nonnulls(roster, active_count, evidence_id, observed_at, fresh_until) IN (0, 5)),
    CHECK (jsonb_typeof(roster) = 'array' AND jsonb_array_length(roster) BETWEEN active_count AND 1000),
    CHECK (fresh_until > observed_at)
);

CREATE TABLE platform_manager.account_snapshots (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    grants_version BIGINT NOT NULL CHECK (grants_version BETWEEN 1 AND 9007199254740991),
    query_hash BYTEA NOT NULL CHECK (octet_length(query_hash) = 32),
    scope_hash BYTEA NOT NULL CHECK (octet_length(scope_hash) = 32),
    snapshot_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    total INTEGER NOT NULL CHECK (total BETWEEN 2 AND 40000)
);
CREATE INDEX account_snapshot_expiry ON platform_manager.account_snapshots (expires_at, id);
CREATE TABLE platform_manager.account_snapshot_items (
    snapshot_id UUID NOT NULL REFERENCES platform_manager.account_snapshots(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 40000),
    item JSONB NOT NULL,
    PRIMARY KEY (snapshot_id, ordinal)
);
CREATE TABLE platform_manager.account_snapshot_admission (
    id BOOLEAN PRIMARY KEY CHECK (id),
    marker BOOLEAN NOT NULL DEFAULT false
);
INSERT INTO platform_manager.account_snapshot_admission VALUES (true);

CREATE TABLE platform_manager.account_refresh_slots (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    component TEXT NOT NULL CHECK (component IN ('license','users')),
    job_id UUID REFERENCES platform_jobs.jobs(id),
    seed_installation_id UUID,
    seed_generation BIGINT,
    seed_authorization_sequence BIGINT,
    PRIMARY KEY (account_id, component),
    CHECK (num_nonnulls(seed_installation_id, seed_generation, seed_authorization_sequence) IN (0, 3)),
    CHECK (seed_generation > 0 AND seed_authorization_sequence > 0)
);
CREATE TABLE platform_manager.account_refresh_work (
    command_id UUID NOT NULL REFERENCES platform_manager.commands(id),
    component TEXT NOT NULL CHECK (component IN ('license','users')),
    job_id UUID NOT NULL REFERENCES platform_jobs.jobs(id),
    PRIMARY KEY (command_id, component)
);

CREATE INDEX account_refresh_active ON platform_jobs.jobs (id)
    WHERE kind = 'accounts.refresh' AND status IN ('queued','running');

CREATE FUNCTION platform_manager.enqueue_account_refresh(account UUID, part TEXT, job UUID)
RETURNS VOID LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    INSERT INTO platform_jobs.jobs(id, kind, payload)
    SELECT job, 'accounts.refresh', jsonb_build_object('account_id', account, 'component', part)
    WHERE EXISTS (SELECT 1 FROM platform_manager.account_refresh_slots s
                      WHERE s.account_id=account AND s.component=part)
      AND EXISTS (SELECT 1 FROM platform_manager.visible_accounts a WHERE a.account_id=account)
$$;
REVOKE ALL ON FUNCTION platform_manager.enqueue_account_refresh(UUID, TEXT, UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_manager.enqueue_account_refresh(UUID, TEXT, UUID) TO platform_jobs;

GRANT SELECT ON platform_manager.account_memberships, platform_manager.visible_accounts TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.account_identities, platform_manager.account_domains, platform_manager.account_rosters TO platform_jobs;
GRANT UPDATE (current_domain, display_name, time_zone, observed_at) ON platform_manager.account_identities TO platform_jobs;
GRANT UPDATE (roster, active_count, evidence_id, observed_at, fresh_until, failure_code) ON platform_manager.account_rosters TO platform_jobs;
GRANT SELECT, INSERT, DELETE ON platform_manager.account_snapshots TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.account_snapshot_items TO platform_jobs;
GRANT SELECT, UPDATE (marker) ON platform_manager.account_snapshot_admission TO platform_jobs;
GRANT SELECT, INSERT ON platform_manager.account_refresh_slots, platform_manager.account_refresh_work TO platform_jobs;
GRANT UPDATE (job_id, seed_installation_id, seed_generation, seed_authorization_sequence) ON platform_manager.account_refresh_slots TO platform_jobs;
