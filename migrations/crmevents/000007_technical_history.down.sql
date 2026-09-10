DROP INDEX IF EXISTS event_jobs_technical_history;
DROP INDEX IF EXISTS event_inbox_operation;
DROP INDEX IF EXISTS event_coverage_retained;
ALTER TABLE event_sources
    DROP COLUMN IF EXISTS events_processed,
    DROP COLUMN IF EXISTS events_inserted,
    DROP COLUMN IF EXISTS events_updated,
    DROP COLUMN IF EXISTS events_deduplicated;
