ALTER TABLE platform_manager.account_snapshots
    ADD COLUMN overview JSONB CHECK (jsonb_typeof(overview) = 'object');

ALTER TABLE platform_manager.account_profiles
    DROP CONSTRAINT account_profiles_classification_check,
    ADD COLUMN partner_name TEXT CONSTRAINT account_profiles_partner_name_length CHECK (char_length(btrim(partner_name)) BETWEEN 1 AND 200),
    ADD CONSTRAINT account_profiles_classification_check
        CHECK (classification IN ('Без партнёра', 'Запрет работы', 'Клиент партнёра')),
    ADD CONSTRAINT account_profiles_partner_name_check
        CHECK ((classification IS NOT DISTINCT FROM 'Клиент партнёра') = (partner_name IS NOT NULL));

GRANT UPDATE (partner_name) ON platform_manager.account_profiles TO platform_jobs;
