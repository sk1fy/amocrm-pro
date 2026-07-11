DROP INDEX inbox_events_cleanup_idx;

CREATE INDEX inbox_events_cleanup_idx
    ON inbox_events (updated_at)
    WHERE status IN ('processed', 'failed', 'dead', 'ignored');
