-- period-v1 (R3/R7): raw CRM history is kept 33 days so a whole previous week
-- and the first days of the previous month stay comparable. Installations on
-- the previous maximum (31, also the previous default) move to the new one;
-- smaller explicit choices are kept. The next accepted sync applies it to CRM Events.
ALTER TABLE settings DROP CONSTRAINT settings_check;
ALTER TABLE settings ADD CONSTRAINT settings_check CHECK (retention_days BETWEEN 2 AND 33 AND retention_days>=initial_days);
UPDATE settings SET retention_days=33, updated_at=now() WHERE retention_days=31;
