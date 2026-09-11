CREATE TABLE panels (
    id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    integration_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120 AND name = btrim(name)),
    employee_ids BIGINT[] NOT NULL CHECK (cardinality(employee_ids) BETWEEN 1 AND 100),
    display_from TEXT NOT NULL CHECK (display_from ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    display_to TEXT NOT NULL CHECK (display_to ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$' AND display_to <> display_from),
    timezone TEXT NOT NULL CHECK (char_length(timezone) BETWEEN 1 AND 64),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    view_key_hash BYTEA NOT NULL CHECK (octet_length(view_key_hash) = 32),
    view_key_version INTEGER NOT NULL CHECK (view_key_version >= 1),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (view_key_hash)
);
CREATE INDEX panels_installation_idx ON panels (installation_id, integration_id);

CREATE TABLE panel_commands (
    command_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    integration_id UUID NOT NULL,
    panel_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('create', 'patch', 'rotate')),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX panel_commands_panel_idx ON panel_commands (panel_id);
