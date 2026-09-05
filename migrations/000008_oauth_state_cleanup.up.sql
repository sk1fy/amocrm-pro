-- Both consumed and unused states expire. The existing partial indexes do not
-- cover cleanup of all states by expires_at.
CREATE INDEX oauth_states_expiry_cleanup_idx ON oauth_states (expires_at);
