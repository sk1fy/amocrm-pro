DROP INDEX event_operation_jobs_job;
DROP INDEX event_sources_retention_turn;
ALTER TABLE event_sources DROP COLUMN retention_checked_at;
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 1 AND 90);
