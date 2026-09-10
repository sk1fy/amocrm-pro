-- CORE-05: terminal command identity may be collected after the same 7-day
-- calendar horizon Core enforces on redelivery. Compact tombstones reject a
-- late Apply of a collected command_id so it cannot become a new accept.
CREATE TABLE event_command_tombstones (
    installation_id uuid NOT NULL REFERENCES event_sources(installation_id),
    command_id text NOT NULL,
    payload_hash bytea NOT NULL,
    command_created_at timestamptz NOT NULL,
    tombstoned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, command_id)
);
CREATE INDEX event_command_tombstones_cleanup
    ON event_command_tombstones (tombstoned_at);
CREATE INDEX event_inbox_created
    ON event_inbox (created_at);
CREATE INDEX event_operations_technical_history
    ON event_operations (updated_at, id)
    WHERE status IN ('completed', 'failed');

CREATE FUNCTION event_inbox_reject_tombstoned_command() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM event_command_tombstones tombstone
        WHERE tombstone.installation_id = NEW.installation_id
          AND tombstone.command_id = NEW.command_id
    ) THEN
        RAISE EXCEPTION 'expired command replay is not accepted'
            USING ERRCODE = '23505';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER event_inbox_reject_tombstoned_command
    BEFORE INSERT ON event_inbox
    FOR EACH ROW
    EXECUTE FUNCTION event_inbox_reject_tombstoned_command();
