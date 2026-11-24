ALTER TABLE platform_company.installation_deal_settings_revisions
    ADD COLUMN owners jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(owners) = 'object');
ALTER TABLE platform_company.generations ADD COLUMN employees jsonb CHECK (jsonb_typeof(employees) = 'array');
