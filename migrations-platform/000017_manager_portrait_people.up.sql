ALTER TABLE platform_manager.account_portraits
    ADD COLUMN people JSONB NOT NULL DEFAULT '[]'::JSONB CHECK (
        CASE WHEN jsonb_typeof(people) = 'array' THEN jsonb_array_length(people) <= 50 ELSE FALSE END
    );

GRANT UPDATE (people) ON platform_manager.account_portraits TO platform_jobs;
