-- Original fair claim selector, before the September 2026 performance changes.
WITH selected AS MATERIALIZED (
    SELECT lane.scope_id, candidate.id
    FROM job_queue_lanes lane
    CROSS JOIN LATERAL (
        SELECT candidate.* FROM (
            SELECT per_installation.*
            FROM installations i
            CROSS JOIN LATERAL (
                SELECT j.id, j.priority, j.run_after, j.created_at
                FROM jobs j
                WHERE j.installation_id=i.id
                  AND j.status IN ('queued', 'retry') AND j.attempts < j.max_attempts
                  AND j.run_after <= statement_timestamp()
                ORDER BY j.priority, j.run_after, j.created_at, j.id
                LIMIT 1 FOR UPDATE OF j SKIP LOCKED
            ) per_installation
            WHERE i.integration_id=lane.integration_id
            UNION ALL
            SELECT platform.* FROM LATERAL (
                SELECT j.id, j.priority, j.run_after, j.created_at
                FROM jobs j
                WHERE lane.integration_id IS NULL AND j.installation_id IS NULL
                  AND j.status IN ('queued', 'retry') AND j.attempts < j.max_attempts
                  AND j.run_after <= statement_timestamp()
                ORDER BY j.priority, j.run_after, j.created_at, j.id
                LIMIT 1 FOR UPDATE OF j SKIP LOCKED
            ) platform
        ) candidate
        ORDER BY candidate.priority, candidate.run_after, candidate.created_at, candidate.id
        LIMIT 1
    ) candidate
    WHERE (
        SELECT count(*) FROM jobs active
        WHERE active.status='processing' AND active.locked_until >= statement_timestamp()
          AND (
            (lane.integration_id IS NULL AND active.installation_id IS NULL)
            OR active.installation_id IN (SELECT id FROM installations WHERE integration_id=lane.integration_id)
          )
    ) < $1
    ORDER BY lane.last_claimed_at, lane.scope_id
    LIMIT 1
), rotated AS (
    UPDATE job_queue_lanes lane SET last_claimed_at=clock_timestamp()
    FROM selected WHERE lane.scope_id=selected.scope_id
)
UPDATE jobs j
SET status='processing', locked_by=$2,
    locked_until=statement_timestamp() + ($3 * interval '1 millisecond'),
    attempts=j.attempts+1, updated_at=statement_timestamp()
FROM selected WHERE j.id=selected.id
RETURNING j.id, j.installation_id, j.type, j.actor_type, j.actor_id, j.resource_type, j.resource_id,
    j.status, j.priority, j.payload, j.result, j.attempts, j.max_attempts, j.run_after,
    j.locked_by, j.locked_until, j.last_error_code, j.last_error_message,
    j.created_at, j.updated_at, j.finished_at
