ALTER TABLE platform_manager.account_profiles DROP CONSTRAINT account_profiles_classification_check;
UPDATE platform_manager.account_profiles SET classification = 'Без партнёра' WHERE classification = 'Свободный клиент';
ALTER TABLE platform_manager.account_profiles ADD CONSTRAINT account_profiles_classification_check
    CHECK (classification IN ('Без партнёра', 'Клиент партнёра', 'Наш клиент', 'Запрет работы'));
