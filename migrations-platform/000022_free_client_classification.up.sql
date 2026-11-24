ALTER TABLE platform_manager.account_profiles DROP CONSTRAINT account_profiles_classification_check;
UPDATE platform_manager.account_profiles SET classification = 'Свободный клиент' WHERE classification = 'Без партнёра';
ALTER TABLE platform_manager.account_profiles ADD CONSTRAINT account_profiles_classification_check
    CHECK (classification IN ('Свободный клиент', 'Клиент партнёра', 'Наш клиент', 'Запрет работы'));
