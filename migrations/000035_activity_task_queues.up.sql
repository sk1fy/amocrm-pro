-- One shared snapshot of an employee's unfinished amoCRM tasks. Every tab and
-- API replica reads this row; a single lease holder refreshes it.
CREATE TABLE activity_task_queues (
 installation_id UUID NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 user_id BIGINT NOT NULL CHECK(user_id>0),
 integration_id UUID NOT NULL REFERENCES integrations(id),
 payload JSONB,
 complete BOOLEAN NOT NULL DEFAULT false,
 fetched_at TIMESTAMPTZ,
 fresh_until TIMESTAMPTZ NOT NULL DEFAULT now(),
 version BIGINT NOT NULL DEFAULT 0,
 lease_token BIGINT NOT NULL DEFAULT 0,
 lease_until TIMESTAMPTZ,
 retry_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 failures INTEGER NOT NULL DEFAULT 0 CHECK(failures>=0),
 error_code TEXT NOT NULL DEFAULT '',
 accessed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(installation_id,user_id),
 CHECK(payload IS NULL OR octet_length(payload::text)<=2097152)
);
CREATE INDEX activity_task_queues_accessed ON activity_task_queues(accessed_at);
-- Completion lease of one task across tabs and replicas. A row lives only
-- while a completion is in flight; an expired row is taken over.
CREATE TABLE activity_task_completions (
 installation_id UUID NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 task_id BIGINT NOT NULL CHECK(task_id>0),
 lease_token UUID NOT NULL,
 lease_until TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(installation_id,task_id)
);
