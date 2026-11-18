ALTER TABLE platform_manager.commands
    ADD COLUMN product_id UUID REFERENCES platform_lifecycle.products(id),
    ADD COLUMN resource_id UUID,
    DROP CONSTRAINT commands_target_shape,
    DROP CONSTRAINT commands_outcome_shape,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind = 'installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(account_id, product_id, resource_id) = 0)
        OR (kind = 'account.refresh' AND account_id IS NOT NULL
            AND num_nonnulls(episode_id, expected_version, product_id, resource_id) = 0)
        OR (kind IN ('account.profile', 'account.portrait', 'account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id, product_id, resource_id) = 0)
        OR (kind = 'account.renewal' AND num_nonnulls(account_id, product_id, expected_version) = 3
            AND num_nonnulls(episode_id, resource_id) = 0)
        OR (kind = 'portrait.template' AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id, account_id, product_id, resource_id) = 0)
        OR (kind = 'portrait.niche.create' AND num_nonnulls(episode_id, account_id, product_id, resource_id, expected_version) = 0)
        OR (kind IN ('portrait.niche.update', 'portrait.niche.delete', 'portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id, account_id, product_id) = 0)),
    ADD CONSTRAINT commands_outcome_shape CHECK (
        (status IS NULL AND response IS NULL AND etag IS NULL)
        OR (status IS NOT NULL AND (
            (kind = 'installation.review' AND status IN (200, 409) AND response IS NOT NULL
                AND octet_length(response) > 0 AND (etag IS NOT NULL) = (status = 200))
            OR (kind = 'account.refresh' AND status = 202 AND response IS NOT NULL AND octet_length(response) > 0 AND etag IS NULL)
            OR (kind NOT IN ('installation.review', 'account.refresh') AND (
                (status IN (200,201,409,422) AND response IS NOT NULL AND octet_length(response) > 0
                    AND (etag IS NOT NULL) = (status IN (200,201)))
                OR (status = 204 AND COALESCE(octet_length(response),0) = 0 AND etag IS NULL))))));

CREATE TABLE platform_manager.account_profiles (
    account_id UUID PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    display_name TEXT CHECK (char_length(display_name) BETWEEN 1 AND 512),
    note TEXT CHECK (char_length(note) <= 4000),
    classification TEXT CHECK (classification IN ('Без партнёра', 'Запрет работы')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991)
);

CREATE TABLE platform_manager.account_change_events (
    id UUID PRIMARY KEY,
    command_id UUID NOT NULL REFERENCES platform_manager.commands(id),
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    product_id UUID REFERENCES platform_lifecycle.products(id),
    kind TEXT NOT NULL CHECK (kind IN ('account_profile_updated', 'portrait_updated', 'access_renewed')),
    actor_id UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    reason TEXT CHECK (char_length(reason) BETWEEN 1 AND 1000),
    details JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object'),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    UNIQUE (command_id, account_id, kind),
    CHECK ((kind = 'access_renewed') = (product_id IS NOT NULL))
);

CREATE TABLE platform_manager.account_event_outbox (
    event_id UUID PRIMARY KEY REFERENCES platform_manager.account_change_events(id),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);

GRANT SELECT, INSERT ON platform_manager.account_profiles TO platform_jobs;
GRANT UPDATE (display_name, note, classification, version) ON platform_manager.account_profiles TO platform_jobs;
GRANT SELECT (id, kind, account_id, product_id, occurred_at, recorded_at) ON platform_manager.account_change_events TO platform_jobs;
GRANT INSERT ON platform_manager.account_change_events, platform_manager.account_event_outbox TO platform_jobs;
