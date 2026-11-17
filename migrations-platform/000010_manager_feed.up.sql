CREATE VIEW platform_manager.business_events AS
SELECT 'lifecycle'::text AS source, h.event_id AS id, h.kind,
       h.occurred_at, h.applied_at AS recorded_at, pair.account_id, pair.product_id, h.episode_id
FROM platform_lifecycle.history h
JOIN platform_lifecycle.episodes ep ON ep.id=h.episode_id
JOIN platform_lifecycle.account_products pair ON pair.installation_id=ep.installation_id
WHERE h.kind IN ('installed','reinstalled','reauthorized','uninstalled')
UNION ALL
SELECT 'review', id, kind, occurred_at, recorded_at, account_id, product_id, episode_id
FROM platform_manager.review_events
UNION ALL
SELECT 'account', id, kind, occurred_at, recorded_at, account_id, product_id, NULL::uuid
FROM platform_manager.account_change_events;

CREATE TABLE platform_manager.feed_snapshots (
    id UUID PRIMARY KEY,
    route TEXT NOT NULL CHECK (route IN ('feed','history')),
    account_id UUID,
    subject_id UUID NOT NULL,
    grants_version BIGINT NOT NULL CHECK (grants_version BETWEEN 1 AND 9007199254740991),
    query_hash BYTEA NOT NULL CHECK (octet_length(query_hash)=32),
    scope_hash BYTEA NOT NULL CHECK (octet_length(scope_hash)=32),
    snapshot_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    total INTEGER NOT NULL CHECK (total BETWEEN 2 AND 40000),
    CHECK (route<>'history' OR account_id IS NOT NULL)
);
CREATE INDEX feed_snapshot_expiry ON platform_manager.feed_snapshots(expires_at,id);
CREATE TABLE platform_manager.feed_snapshot_items (
    snapshot_id UUID NOT NULL REFERENCES platform_manager.feed_snapshots(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 40000),
    source TEXT NOT NULL CHECK (source IN ('lifecycle','review','account')),
    event_id UUID NOT NULL,
    PRIMARY KEY (snapshot_id,ordinal),
    UNIQUE (snapshot_id,source,event_id)
);
GRANT SELECT ON platform_manager.business_events TO platform_jobs;
GRANT SELECT,INSERT,DELETE ON platform_manager.feed_snapshots TO platform_jobs;
GRANT SELECT,INSERT ON platform_manager.feed_snapshot_items TO platform_jobs;
