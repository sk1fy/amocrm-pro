-- Use 33 days for new sources. Initial history stays at most 744 hours.
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 33);
ALTER TABLE event_sources ALTER COLUMN retention_days SET DEFAULT 33;
