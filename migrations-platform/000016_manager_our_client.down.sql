DO $$
BEGIN
    IF EXISTS (
        SELECT FROM platform_manager.account_profiles WHERE classification = 'Наш клиент'
    ) THEN
        RAISE EXCEPTION 'our client assignments cannot be represented in the previous schema';
    END IF;
END;
$$;

ALTER TABLE platform_manager.account_profiles
    DROP CONSTRAINT account_profiles_classification_check,
    ADD CONSTRAINT account_profiles_classification_check
        CHECK (classification IN ('Без партнёра', 'Запрет работы', 'Клиент партнёра'));
