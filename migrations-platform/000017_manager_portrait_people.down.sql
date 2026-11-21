DO $$
BEGIN
    IF EXISTS (SELECT FROM platform_manager.account_portraits WHERE people <> '[]'::JSONB) THEN
        RAISE EXCEPTION 'portrait contacts cannot be represented in the previous schema';
    END IF;
END;
$$;

ALTER TABLE platform_manager.account_portraits DROP COLUMN people;
