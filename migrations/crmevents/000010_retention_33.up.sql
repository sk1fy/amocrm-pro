-- period-v1 (R3/R7): keep raw events 33 days. Sources on the previous maximum
-- (31, also the previous default) move to 33; smaller explicit choices are kept.
-- Initial history stays at most 744 hours (31 days).
ALTER TABLE event_sources DROP CONSTRAINT event_sources_retention_days_check;
ALTER TABLE event_sources ADD CONSTRAINT event_sources_retention_days_check CHECK (retention_days BETWEEN 2 AND 33);
ALTER TABLE event_sources ALTER COLUMN retention_days SET DEFAULT 33;
UPDATE event_sources SET retention_days=33, updated_at=now() WHERE retention_days=31;
