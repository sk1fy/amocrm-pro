ALTER TABLE event_sources DROP CONSTRAINT event_sources_initial_hours_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_initial_hours_check CHECK (initial_hours BETWEEN 1 AND 744);
ALTER TABLE event_sources ALTER COLUMN initial_hours SET DEFAULT 744;
ALTER TABLE event_sources ALTER COLUMN retention_days SET DEFAULT 31;
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 31);
