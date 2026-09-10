-- CORE-05: calendar redelivery horizon is receipt.created_at + 7 days, not
-- attempt count. Terminal Core history may be collected at the same floor.
ALTER TABLE activity_command_outbox
    DROP CONSTRAINT activity_command_outbox_status_check;
ALTER TABLE activity_command_outbox
    ADD CONSTRAINT activity_command_outbox_status_check
    CHECK (status IN ('pending_delivery', 'delivering', 'accepted', 'failed', 'expired'));

CREATE INDEX activity_receipts_created_idx
    ON activity_command_receipts (created_at);

CREATE INDEX webhook_event_tombstones_cleanup_idx
    ON webhook_event_tombstones (last_seen_at);

CREATE INDEX workflow_runs_cleanup_idx
    ON workflow_runs (finished_at)
    WHERE status IN ('completed', 'failed', 'dead') AND finished_at IS NOT NULL;

CREATE INDEX outbound_effects_cleanup_idx
    ON outbound_effects (updated_at)
    WHERE state IN ('observed', 'no_effect', 'failed', 'expired');

CREATE INDEX audit_log_cleanup_idx
    ON audit_log (created_at);
