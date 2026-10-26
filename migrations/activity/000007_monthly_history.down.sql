-- Explicitly reduce configured horizons before downgrade; never silently truncate settings.
ALTER TABLE settings DROP CONSTRAINT settings_initial_days_check;
ALTER TABLE settings DROP CONSTRAINT settings_check;
ALTER TABLE settings ADD CONSTRAINT settings_initial_days_check CHECK (initial_days BETWEEN 1 AND 7);
ALTER TABLE settings ADD CONSTRAINT settings_check CHECK (retention_days BETWEEN 2 AND 30 AND retention_days>=initial_days);
