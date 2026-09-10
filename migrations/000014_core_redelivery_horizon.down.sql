DROP INDEX IF EXISTS audit_log_cleanup_idx;
DROP INDEX IF EXISTS outbound_effects_cleanup_idx;
DROP INDEX IF EXISTS workflow_runs_cleanup_idx;
DROP INDEX IF EXISTS webhook_event_tombstones_cleanup_idx;
DROP INDEX IF EXISTS activity_receipts_created_idx;

UPDATE activity_command_outbox SET status='failed' WHERE status='expired';

ALTER TABLE activity_command_outbox
    DROP CONSTRAINT activity_command_outbox_status_check;
ALTER TABLE activity_command_outbox
    ADD CONSTRAINT activity_command_outbox_status_check
    CHECK (status IN ('pending_delivery', 'delivering', 'accepted', 'failed'));
