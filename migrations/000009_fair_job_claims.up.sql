-- Queue scheduling state is separate from integration credentials so claims do
-- not contend with capability/OAuth row locks. The nil UUID is the platform lane.
CREATE TABLE job_queue_lanes (
    scope_id UUID PRIMARY KEY,
    integration_id UUID UNIQUE REFERENCES integrations(id) ON DELETE CASCADE,
    last_claimed_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    CONSTRAINT job_queue_lane_scope CHECK (
        (integration_id IS NOT NULL AND scope_id = integration_id AND scope_id <> '00000000-0000-0000-0000-000000000000')
        OR (integration_id IS NULL AND scope_id = '00000000-0000-0000-0000-000000000000')
    )
);
INSERT INTO job_queue_lanes(scope_id) VALUES ('00000000-0000-0000-0000-000000000000');
INSERT INTO job_queue_lanes(scope_id, integration_id) SELECT id, id FROM integrations;
CREATE FUNCTION create_integration_job_queue_lane() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO job_queue_lanes(scope_id, integration_id) VALUES (NEW.id, NEW.id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER integrations_create_job_queue_lane AFTER INSERT ON integrations
    FOR EACH ROW EXECUTE FUNCTION create_integration_job_queue_lane();
CREATE INDEX jobs_installation_ready_idx ON jobs(installation_id, priority, run_after, created_at, id)
    WHERE status IN ('queued', 'retry') AND attempts < max_attempts;
CREATE INDEX jobs_installation_processing_idx ON jobs(installation_id, locked_until)
    WHERE status = 'processing';
