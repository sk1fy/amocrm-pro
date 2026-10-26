-- Configure smaller initial horizons before downgrade.
ALTER TABLE event_sources DROP CONSTRAINT event_sources_initial_hours_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_initial_hours_check CHECK (initial_hours BETWEEN 1 AND 168);
ALTER TABLE event_sources ALTER COLUMN initial_hours SET DEFAULT 48;
ALTER TABLE event_sources ALTER COLUMN retention_days SET DEFAULT 7;
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 30);
