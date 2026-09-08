-- Enrichment is owner-local current-state data. It must not rewrite crm_events
-- history and must not take the collector source lease.
CREATE TABLE event_enrichment_objects (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    object_kind text NOT NULL CHECK (object_kind IN ('note','task','entity','pipeline','custom_field')),
    object_key text NOT NULL CHECK (char_length(object_key) BETWEEN 1 AND 256),
    parent_type text NOT NULL DEFAULT '',
    parent_id bigint NOT NULL DEFAULT 0 CHECK (parent_id >= 0),
    object_id bigint NOT NULL DEFAULT 0 CHECK (object_id >= 0),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','ready','unavailable','retry','error')),
    reason_code text NOT NULL DEFAULT 'not_loaded',
    source text NOT NULL DEFAULT '',
    payload jsonb NOT NULL DEFAULT 'null'::jsonb,
    payload_hash bytea,
    fetched_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    run_after timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, object_kind, object_key)
);
CREATE INDEX event_enrichment_claim ON event_enrichment_objects(installation_id, object_kind, run_after)
    WHERE state IN ('pending','retry');
CREATE TABLE event_enrichment_links (
    installation_id uuid NOT NULL,
    event_id text NOT NULL,
    object_kind text NOT NULL,
    object_key text NOT NULL,
    PRIMARY KEY (installation_id, event_id, object_kind, object_key),
    FOREIGN KEY (installation_id, event_id) REFERENCES crm_events(installation_id, event_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, object_kind, object_key) REFERENCES event_enrichment_objects(installation_id, object_kind, object_key) ON DELETE CASCADE
);
CREATE INDEX event_enrichment_links_object ON event_enrichment_links(installation_id, object_kind, object_key);
