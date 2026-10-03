-- Installation-local pause: only admission/new assignment dispatch stops.
-- Delivery and observations of existing ambiguous effects continue.
CREATE TABLE distribution_admin_pauses (
 installation_id UUID PRIMARY KEY REFERENCES installations(id),
 paused BOOLEAN NOT NULL DEFAULT false,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER distribution_admin_pauses_updated BEFORE UPDATE ON distribution_admin_pauses
 FOR EACH ROW EXECUTE FUNCTION set_updated_at();
INSERT INTO distribution_admin_pauses(installation_id) SELECT id FROM installations;
CREATE FUNCTION initialize_distribution_admin_pause() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO distribution_admin_pauses(installation_id) VALUES(NEW.id); RETURN NEW; END; $$;
CREATE TRIGGER installation_distribution_admin_pause AFTER INSERT ON installations
 FOR EACH ROW EXECUTE FUNCTION initialize_distribution_admin_pause();
CREATE INDEX distribution_admin_events_scope ON distribution_event_outbox((scope->>'installationId'),created_at DESC,message_id DESC);
CREATE INDEX distribution_admin_operations_scope ON distribution_operations(installation_id,created_at DESC,id DESC);
