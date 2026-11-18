CREATE SCHEMA platform_access;
CREATE VIEW platform_access.runtime_probe AS SELECT true AS ready;
GRANT USAGE ON SCHEMA platform_access TO platform_access;
GRANT SELECT ON platform_access.runtime_probe TO platform_access;

CREATE SCHEMA platform_jobs;
CREATE TABLE platform_jobs.jobs (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 80),
    payload JSONB NOT NULL CHECK (octet_length(payload::text) <= 65536),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'dead')),
    available_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    attempt INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 10),
    lease_until TIMESTAMPTZ,
    CHECK (attempt BETWEEN 0 AND max_attempts),
    CHECK (status <> 'running' OR attempt > 0),
    CHECK (status <> 'queued' OR attempt < max_attempts),
    CHECK ((status = 'running') = (lease_until IS NOT NULL))
);
CREATE INDEX jobs_available_idx ON platform_jobs.jobs (available_at, id) WHERE status = 'queued';
CREATE INDEX jobs_lease_idx ON platform_jobs.jobs (lease_until, id) WHERE status = 'running';

GRANT USAGE ON SCHEMA platform_jobs TO platform_jobs;
GRANT SELECT ON platform_jobs.jobs TO platform_jobs;
GRANT UPDATE (status, available_at, attempt, lease_until) ON platform_jobs.jobs TO platform_jobs;
