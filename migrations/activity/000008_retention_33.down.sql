-- The 31-day bound cannot hold 32..33; clamp them back to the previous maximum.
UPDATE settings SET retention_days=31, updated_at=now() WHERE retention_days>31;
ALTER TABLE settings DROP CONSTRAINT settings_check;
ALTER TABLE settings ADD CONSTRAINT settings_check CHECK (retention_days BETWEEN 2 AND 31 AND retention_days>=initial_days);
