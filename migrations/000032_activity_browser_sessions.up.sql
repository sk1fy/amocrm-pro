CREATE TABLE activity_browser_sessions (
 token_hash BYTEA PRIMARY KEY CHECK(octet_length(token_hash)=32),
 installation_id UUID NOT NULL REFERENCES installations(id),
 integration_id UUID NOT NULL REFERENCES integrations(id),
 actor_id BIGINT NOT NULL CHECK(actor_id>0),
 expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX activity_browser_sessions_expiry ON activity_browser_sessions(expires_at);
