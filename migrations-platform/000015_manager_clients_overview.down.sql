-- The earlier schema cannot represent a partner assignment. Refuse to drop it.
DO $$
BEGIN
    IF EXISTS (
        SELECT FROM platform_manager.account_profiles WHERE partner_name IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'account partner assignments cannot be represented in the previous schema';
    END IF;
END;
$$;

ALTER TABLE platform_manager.account_profiles
    DROP CONSTRAINT account_profiles_partner_name_check,
    DROP CONSTRAINT account_profiles_classification_check,
    DROP COLUMN partner_name,
    ADD CONSTRAINT account_profiles_classification_check
        CHECK (classification IN ('Без партнёра', 'Запрет работы'));

ALTER TABLE platform_manager.account_snapshots DROP COLUMN overview;
