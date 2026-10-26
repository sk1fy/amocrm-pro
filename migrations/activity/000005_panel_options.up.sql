ALTER TABLE panels ADD COLUMN options JSONB NOT NULL DEFAULT '{"pipeline_ids":[],"operators":[],"departments":[]}'::jsonb CHECK (jsonb_typeof(options)='object');
