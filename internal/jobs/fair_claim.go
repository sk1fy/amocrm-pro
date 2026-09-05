package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Separate from the test lock, account rate limits, and other application locks.
const fairClaimLockID int64 = 6_143_721_951_403_017_411

// ClaimFairWithObserver rotates between integrations, preserving priority order
// within each integration. The cap counts live leases across all installations
// and all worker replicas. Jobs without an installation share a platform lane.
// Every production claimant must use this method with the same cap.
func (s *Store) ClaimFairWithObserver(ctx context.Context, workerID string, limit, reapLimit int,
	lease time.Duration, integrationConcurrency int, observer FailureObserver,
) ([]Job, error) {
	if workerID == "" || limit < 1 || reapLimit < 1 || lease < time.Millisecond || integrationConcurrency < 1 {
		return nil, errors.New("worker id, positive claim, reap and integration limits, and lease of at least 1ms are required")
	}
	// ReadCommitted is intentional: capacity must be read AFTER obtaining the
	// scheduler lock, in a new statement snapshot that sees the preceding claim.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin fair claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, fairClaimLockID); err != nil {
		return nil, fmt.Errorf("lock fair claim scheduler: %w", err)
	}
	// The platform lane has no integration insert trigger; also restore it after
	// maintenance truncates queue state together with integrations.
	if _, err := tx.Exec(ctx, `INSERT INTO job_queue_lanes(scope_id)
		VALUES ('00000000-0000-0000-0000-000000000000') ON CONFLICT DO NOTHING`); err != nil {
		return nil, fmt.Errorf("ensure platform queue lane: %w", err)
	}
	if err := reapExpired(ctx, tx, reapLimit, observer); err != nil {
		return nil, err
	}
	if err := reapExhausted(ctx, tx, reapLimit, observer); err != nil {
		return nil, err
	}
	claimed := make([]Job, 0, limit)
	for range limit {
		job, err := scanJob(tx.QueryRow(ctx, fairClaimQuery, integrationConcurrency, workerID, lease.Milliseconds()))
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("fair claim job: %w", err)
		}
		claimed = append(claimed, job)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit fair claim: %w", err)
	}
	return claimed, nil
}

const fairClaimQuery = `
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
RETURNING ` + `j.id, j.installation_id, j.type, j.actor_type, j.actor_id, j.resource_type, j.resource_id,
    j.status, j.priority, j.payload, j.result, j.attempts, j.max_attempts, j.run_after,
    j.locked_by, j.locked_until, j.last_error_code, j.last_error_message,
    j.created_at, j.updated_at, j.finished_at`
