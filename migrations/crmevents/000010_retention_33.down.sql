-- The 31-day bound cannot hold 32..33; clamp them back to the previous maximum.
UPDATE event_sources SET retention_days=31, updated_at=now() WHERE retention_days>31;
ALTER TABLE event_sources ALTER COLUMN retention_days SET DEFAULT 31;
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 31);
