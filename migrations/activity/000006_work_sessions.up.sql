CREATE TABLE task_panel_settings (
 installation_id UUID NOT NULL,
 integration_id UUID NOT NULL,
 settings JSONB NOT NULL CHECK (jsonb_typeof(settings)='object'),
 revision BIGINT NOT NULL CHECK (revision > 0),
 PRIMARY KEY(installation_id,integration_id)
);
CREATE TABLE work_sessions (
 id UUID PRIMARY KEY,
 installation_id UUID NOT NULL,
 integration_id UUID NOT NULL,
 employee_id BIGINT NOT NULL CHECK(employee_id>0),
 kind TEXT NOT NULL CHECK(kind IN ('work','activity','pause')),
 started_at TIMESTAMPTZ NOT NULL,
 stopped_at TIMESTAMPTZ,
 context JSONB NOT NULL CHECK (jsonb_typeof(context)='object'),
 CHECK(stopped_at IS NULL OR stopped_at>=started_at)
);
CREATE UNIQUE INDEX work_sessions_one_open ON work_sessions(installation_id,integration_id,employee_id) WHERE stopped_at IS NULL;
CREATE INDEX work_sessions_history ON work_sessions(installation_id,integration_id,employee_id,started_at);
CREATE TABLE session_commands (
 installation_id UUID NOT NULL,
 integration_id UUID NOT NULL,
 employee_id BIGINT NOT NULL,
 command_id UUID NOT NULL,
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 result JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(installation_id,integration_id,employee_id,command_id)
);
