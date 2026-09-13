CREATE TABLE admin_commands (
    id UUID PRIMARY KEY,
    key_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    actor_id TEXT NOT NULL CHECK (btrim(actor_id) <> ''),
    target_type TEXT NOT NULL CHECK (target_type IN ('installation', 'integration', 'job', 'delivery')),
    target_id TEXT NOT NULL,
    command TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted', 'pending', 'running', 'succeeded', 'failed', 'partial', 'unknown_outcome')),
    outcome TEXT NOT NULL DEFAULT '',
    result JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(result) = 'object'),
    error JSONB CHECK (error IS NULL OR jsonb_typeof(error) = 'object'),
    job_id UUID REFERENCES jobs(id) ON DELETE SET NULL,
    installation_id UUID REFERENCES installations(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE TRIGGER admin_commands_set_updated_at BEFORE UPDATE ON admin_commands
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE UNIQUE INDEX admin_commands_pending_target_idx ON admin_commands(target_type, target_id)
    WHERE state IN ('accepted', 'pending', 'running');
CREATE INDEX admin_commands_job_idx ON admin_commands(job_id) WHERE job_id IS NOT NULL;
