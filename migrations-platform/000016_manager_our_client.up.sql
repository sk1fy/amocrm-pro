ALTER TABLE platform_manager.account_profiles
    DROP CONSTRAINT account_profiles_classification_check,
    ADD CONSTRAINT account_profiles_classification_check
        CHECK (classification IN ('Без партнёра', 'Запрет работы', 'Клиент партнёра', 'Наш клиент'));
