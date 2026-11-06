-- Activity supplies the 33-day default when it creates settings.
ALTER TABLE settings DROP CONSTRAINT settings_check;
ALTER TABLE settings ADD CONSTRAINT settings_check CHECK (retention_days BETWEEN 2 AND 33 AND retention_days>=initial_days);
