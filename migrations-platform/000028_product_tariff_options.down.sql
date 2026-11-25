LOCK TABLE platform_lifecycle.products IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT FROM platform_lifecycle.products WHERE tariff_options <> '[]'::jsonb OR trial_extension_days <> 7) THEN
        RAISE EXCEPTION 'cannot drop configured product tariff options';
    END IF;
END $$;
ALTER TABLE platform_lifecycle.products DROP COLUMN tariff_options, DROP COLUMN trial_extension_days;
DROP FUNCTION platform_manager.record_free_category(uuid,uuid);
