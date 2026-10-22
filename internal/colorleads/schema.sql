-- Candidate schema only. Coordinate production migration numbering with PV-03.
-- No production installation/backfill is performed by this package.
CREATE TABLE color_leads_documents (
 installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE RESTRICT,
 installation_generation bigint NOT NULL CHECK (installation_generation > 0),
 settings jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
 settings_revision bigint NOT NULL DEFAULT 0 CHECK (settings_revision >= 0),
 colors jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(colors) = 'array'),
 colors_revision bigint NOT NULL DEFAULT 0 CHECK (colors_revision >= 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (installation_id, installation_generation)
);
CREATE TABLE color_leads_imports (
 installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE RESTRICT,
 installation_generation bigint NOT NULL CHECK(installation_generation > 0),
 transfer_id text NOT NULL,
 digest text NOT NULL CHECK(length(digest)=64),
 source_digest text NOT NULL CHECK(length(source_digest)=64),
 routing_epoch bigint NOT NULL CHECK(routing_epoch > 0),
 actor text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(installation_id,installation_generation,transfer_id)
);
