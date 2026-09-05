-- The reaper runs on every claim, including when no attempts are exhausted.
-- Keep ordinary ready/scheduled jobs out of this scan.
CREATE INDEX jobs_exhausted_idx ON jobs(run_after, created_at)
    WHERE status IN ('queued', 'retry') AND attempts >= max_attempts;

-- The installation-prefixed index can locate NULL installations, but the
-- measured platform plan sorts all matching jobs before LIMIT 1. This index
-- supplies the platform priority order directly.
CREATE INDEX jobs_platform_ready_idx ON jobs(priority, run_after, created_at, id)
    WHERE installation_id IS NULL AND status IN ('queued', 'retry') AND attempts < max_attempts;
