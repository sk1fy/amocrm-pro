DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM panel_commands WHERE kind = 'patch') THEN
        RAISE EXCEPTION 'cannot drop panel_commands.result while patch receipts exist';
    END IF;
END $$;

ALTER TABLE panel_commands
    DROP COLUMN result;
