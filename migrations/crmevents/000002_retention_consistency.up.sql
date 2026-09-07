-- Never silently shorten an existing source's retention. Operators must review
-- legacy values before retrying this migration; this transaction changes no data.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM event_sources WHERE retention_days NOT BETWEEN 2 AND 30) THEN
        RAISE EXCEPTION 'CRM Events v0 retention must be 2..30 days; review legacy event_sources.retention_days explicitly before retrying migration';
    END IF;
END $$;
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 30);
ALTER TABLE event_sources ADD COLUMN retention_checked_at timestamptz;
CREATE INDEX event_sources_retention_turn ON event_sources(retention_checked_at NULLS FIRST, installation_id);
CREATE INDEX event_operation_jobs_job ON event_operation_jobs(job_id);
