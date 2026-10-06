-- Durable Digital Pipeline trigger inbox. The HTTP receiver authenticates the
-- scoped installation webhook key, validates and redacts the payload (the key is
-- never stored), dedupes and enqueues a job, then returns fast. The worker reads
-- the lead, normalizes and writes the distribution outbox with retry.
CREATE TABLE distribution_dp_inbox (
  id UUID PRIMARY KEY,
  dedup_key BYTEA NOT NULL UNIQUE,
  installation_id UUID NOT NULL REFERENCES installations(id),
  account_id BIGINT NOT NULL,
  lead_id BIGINT NOT NULL,
  pipeline_id BIGINT NOT NULL,
  status_id BIGINT NOT NULL,
  event_type INTEGER NOT NULL,
  direction TEXT NOT NULL,
  group_id UUID NOT NULL,
  occurred_at BIGINT NOT NULL,
  payload JSONB NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processed','ignored')),
  job_id UUID,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  processed_at TIMESTAMPTZ
);
CREATE INDEX distribution_dp_inbox_pending ON distribution_dp_inbox(created_at, id) WHERE state = 'pending';
