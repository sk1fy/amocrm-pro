CREATE TABLE platform_manager.portrait_niches (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 1000),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991),
    removed_at TIMESTAMPTZ,
    merged_into UUID REFERENCES platform_manager.portrait_niches(id),
    CHECK (merged_into IS NULL OR (removed_at IS NOT NULL AND merged_into <> id))
);
CREATE UNIQUE INDEX portrait_niches_current_name ON platform_manager.portrait_niches (lower(name))
    WHERE removed_at IS NULL;

CREATE TABLE platform_manager.portrait_niche_revision (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991)
);
INSERT INTO platform_manager.portrait_niche_revision DEFAULT VALUES;

CREATE TABLE platform_manager.portrait_template (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991),
    topics JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(topics) = 'array')
);
INSERT INTO platform_manager.portrait_template DEFAULT VALUES;

CREATE TABLE platform_manager.account_portraits (
    account_id UUID PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 9007199254740991),
    niche_id UUID REFERENCES platform_manager.portrait_niches(id),
    site TEXT,
    rating INTEGER CHECK (rating BETWEEN 1 AND 5),
    work_types TEXT[],
    customer_types TEXT[],
    maturity JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(maturity) = 'object'),
    topic_notes JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(topic_notes) = 'object')
);
CREATE INDEX account_portraits_by_niche ON platform_manager.account_portraits(niche_id, account_id)
    WHERE niche_id IS NOT NULL;

CREATE TABLE platform_manager.portrait_audit_dates (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    audit_date DATE NOT NULL,
    id UUID NOT NULL UNIQUE,
    note TEXT CHECK (char_length(note) <= 4000),
    PRIMARY KEY (account_id, audit_date)
);

GRANT SELECT ON platform_manager.portrait_niches, platform_manager.portrait_niche_revision,
    platform_manager.portrait_template, platform_manager.account_portraits,
    platform_manager.portrait_audit_dates TO platform_jobs;
GRANT INSERT ON platform_manager.portrait_niches, platform_manager.account_portraits,
    platform_manager.portrait_audit_dates TO platform_jobs;
GRANT UPDATE (name, version, removed_at, merged_into) ON platform_manager.portrait_niches TO platform_jobs;
GRANT UPDATE (version) ON platform_manager.portrait_niche_revision TO platform_jobs;
GRANT UPDATE (version, topics) ON platform_manager.portrait_template TO platform_jobs;
GRANT UPDATE (version, niche_id, site, rating, work_types, customer_types,
    maturity, topic_notes) ON platform_manager.account_portraits TO platform_jobs;
