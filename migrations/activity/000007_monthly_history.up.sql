ALTER TABLE settings DROP CONSTRAINT settings_initial_days_check;
ALTER TABLE settings DROP CONSTRAINT settings_check;
ALTER TABLE settings ADD CONSTRAINT settings_initial_days_check CHECK (initial_days BETWEEN 1 AND 31);
ALTER TABLE settings ADD CONSTRAINT settings_check CHECK (retention_days BETWEEN 2 AND 31 AND retention_days>=initial_days);
