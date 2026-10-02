DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM distribution_result_outbox WHERE attempts>0) OR EXISTS(SELECT 1 FROM distribution_source_heads) OR EXISTS(SELECT 1 FROM webhook_consumer_receipts) OR EXISTS(SELECT 1 FROM distribution_event_outbox) OR EXISTS(SELECT 1 FROM distribution_recovery_scans) THEN
  RAISE EXCEPTION 'consumer/delivery/recovery identities require explicit export before rollback';
 END IF;
END $$;
DROP TABLE distribution_event_outbox;
DROP TABLE distribution_recovery_scans;
DROP TABLE distribution_source_heads;
DROP SEQUENCE distribution_observation_revision_seq;
DROP TABLE webhook_consumer_receipts;
ALTER TABLE webhook_deliveries DROP COLUMN consumer_snapshot;
ALTER TABLE distribution_result_outbox DROP CONSTRAINT distribution_result_lease_pair;
ALTER TABLE distribution_result_outbox DROP CONSTRAINT distribution_result_outbox_state_check;
ALTER TABLE distribution_result_outbox DROP COLUMN attempts,DROP COLUMN next_attempt_at,DROP COLUMN lease_token,DROP COLUMN lease_until,DROP COLUMN acknowledged_at,DROP COLUMN ack_receipt_id,DROP COLUMN error_code;
ALTER TABLE distribution_result_outbox ADD CONSTRAINT distribution_result_outbox_state_check CHECK(state IN ('pending','acknowledged'));
