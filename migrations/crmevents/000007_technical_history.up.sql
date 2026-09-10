-- Bounded indexes for owner technical-history GC. Product event retention
-- (2..30 days, migration 000002) is unchanged. Completed/failed jobs live at
-- least 7 days. Paused work and command receipts remain until a bounded Core
-- redelivery protocol makes their cleanup safe.
CREATE INDEX event_jobs_technical_history ON event_jobs (updated_at, id)
    WHERE status IN ('completed', 'failed');
CREATE INDEX event_inbox_operation ON event_inbox (operation_id);
CREATE INDEX event_coverage_retained ON event_coverage (installation_id, window_to);

-- Cumulative page counters survive job GC so Prometheus CounterValue series
-- do not reset when completed history is deleted.
ALTER TABLE event_sources
    ADD COLUMN events_processed bigint NOT NULL DEFAULT 0 CHECK (events_processed >= 0),
    ADD COLUMN events_inserted bigint NOT NULL DEFAULT 0 CHECK (events_inserted >= 0),
    ADD COLUMN events_updated bigint NOT NULL DEFAULT 0 CHECK (events_updated >= 0),
    ADD COLUMN events_deduplicated bigint NOT NULL DEFAULT 0 CHECK (events_deduplicated >= 0);

UPDATE event_sources AS source SET
    events_processed = totals.processed,
    events_inserted = totals.inserted,
    events_updated = totals.updated,
    events_deduplicated = totals.deduplicated
FROM (
    SELECT installation_id,
           coalesce(sum(processed), 0) AS processed,
           coalesce(sum(inserted), 0) AS inserted,
           coalesce(sum(updated), 0) AS updated,
           coalesce(sum(deduplicated), 0) AS deduplicated
    FROM event_jobs
    GROUP BY installation_id
) AS totals
WHERE source.installation_id = totals.installation_id;
