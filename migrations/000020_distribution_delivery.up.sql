-- NULL identifies pre-upgrade deliveries; new ingress always freezes an array.
ALTER TABLE webhook_deliveries ADD COLUMN consumer_snapshot JSONB;
CREATE TABLE webhook_consumer_receipts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 event_id UUID NOT NULL,
 source_inbox_event_id UUID,
 installation_id UUID NOT NULL REFERENCES installations(id),
 consumer_id TEXT NOT NULL CHECK(consumer_id IN ('core-lead-status-v1','core-lead-distribution-v1')),
 source_fingerprint BYTEA NOT NULL CHECK(octet_length(source_fingerprint)=32),
 frozen_payload JSONB NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processed','ignored','blocked')),
 disposition TEXT,
 job_id UUID,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), finished_at TIMESTAMPTZ,
 FOREIGN KEY(source_inbox_event_id,installation_id) REFERENCES inbox_events(id,installation_id) ON DELETE SET NULL (source_inbox_event_id),
 FOREIGN KEY(job_id,installation_id) REFERENCES jobs(id,installation_id) ON DELETE SET NULL (job_id),
 UNIQUE(event_id,consumer_id), UNIQUE(installation_id,consumer_id,source_fingerprint)
);
CREATE INDEX webhook_consumer_pending ON webhook_consumer_receipts(consumer_id,created_at) WHERE state='pending';
CREATE SEQUENCE distribution_observation_revision_seq MAXVALUE 9007199254740991 NO CYCLE;
CREATE TABLE distribution_source_heads (
 account_id BIGINT NOT NULL, lead_id BIGINT NOT NULL,
 observation_revision BIGINT NOT NULL CHECK(observation_revision BETWEEN 1 AND 9007199254740991),
 snapshot JSONB, absent BOOLEAN NOT NULL DEFAULT false, absence_reason TEXT, deleted BOOLEAN NOT NULL DEFAULT false,
 observed_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(account_id,lead_id)
);
CREATE TABLE distribution_event_outbox (
 message_id UUID PRIMARY KEY,
 consumer_receipt_id UUID REFERENCES webhook_consumer_receipts(id),
 scope JSONB NOT NULL, payload JSONB NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivering','acknowledged','blocked')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 lease_token UUID, lease_until TIMESTAMPTZ,
 acknowledged_at TIMESTAMPTZ, ack_receipt_id UUID, error_code TEXT,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 CHECK((lease_token IS NULL)=(lease_until IS NULL)),
 UNIQUE(consumer_receipt_id)
);
ALTER TABLE distribution_result_outbox DROP CONSTRAINT distribution_result_outbox_state_check;
ALTER TABLE distribution_result_outbox ADD CONSTRAINT distribution_result_outbox_state_check CHECK(state IN ('pending','delivering','acknowledged','blocked'));
ALTER TABLE distribution_result_outbox ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distribution_result_outbox ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE distribution_result_outbox ADD COLUMN lease_token UUID;
ALTER TABLE distribution_result_outbox ADD COLUMN lease_until TIMESTAMPTZ;
ALTER TABLE distribution_result_outbox ADD COLUMN acknowledged_at TIMESTAMPTZ;
ALTER TABLE distribution_result_outbox ADD COLUMN ack_receipt_id UUID;
ALTER TABLE distribution_result_outbox ADD COLUMN error_code TEXT;
ALTER TABLE distribution_result_outbox ADD CONSTRAINT distribution_result_lease_pair CHECK((lease_token IS NULL)=(lease_until IS NULL));
CREATE TABLE distribution_recovery_scans (
 id UUID PRIMARY KEY, binding_id UUID NOT NULL REFERENCES distribution_bindings(id),
 scope JSONB NOT NULL, window_from TIMESTAMPTZ NOT NULL, window_to TIMESTAMPTZ NOT NULL,
 cursor_page INTEGER NOT NULL DEFAULT 1 CHECK(cursor_page BETWEEN 1 AND 21),
 page_ids JSONB, page_has_next BOOLEAN, item_cursor INTEGER NOT NULL DEFAULT 0 CHECK(item_cursor BETWEEN 0 AND 250),
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 max_pages INTEGER NOT NULL DEFAULT 20 CHECK(max_pages BETWEEN 1 AND 20),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','scanning','completed','blocked')),
 lease_token UUID, lease_until TIMESTAMPTZ,
 gap_reason TEXT NOT NULL DEFAULT 'historical_transitions_unrecoverable',
 error_code TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 CHECK(window_to>window_from), CHECK(window_to-window_from <= interval '24 hours'),
 CHECK((lease_token IS NULL)=(lease_until IS NULL))
);
ALTER TABLE distribution_event_outbox ADD COLUMN recovery_scan_id UUID REFERENCES distribution_recovery_scans(id);
ALTER TABLE distribution_event_outbox ADD COLUMN recovery_lead_id BIGINT CHECK(recovery_lead_id>0);
CREATE UNIQUE INDEX distribution_scan_lead_delivery ON distribution_event_outbox(recovery_scan_id,recovery_lead_id);
CREATE INDEX distribution_event_delivery_ready ON distribution_event_outbox(next_attempt_at,created_at) WHERE state IN ('pending','delivering');
CREATE INDEX distribution_result_delivery_ready ON distribution_result_outbox(next_attempt_at,created_at) WHERE state IN ('pending','delivering');
