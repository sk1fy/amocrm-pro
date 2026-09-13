package jobs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrRetryNotAllowed = errors.New("job type or state does not support safe retry")

func SafeRetryType(kind string) bool { return kind == "webhook.reconcile" || kind == "widget.ping" }

func SafeRetryReason(kind string, status Status, attempts int, targetActive bool) string {
	if !SafeRetryType(kind) {
		return "unsupported_type"
	}
	if status != StatusFailed && status != StatusDead {
		return "not_failed"
	}
	if !targetActive {
		return "target_inactive"
	}
	if attempts > 2147483642 {
		return "attempt_limit"
	}
	return ""
}

// RetrySafeTx preserves the job's principal, payload, correlation ID and attempt
// history. Only convergent reconciliation and side-effect-free ping are allowed.
func (s *Store) RetrySafeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor string) (Job, error) {
	job, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if SafeRetryReason(job.Type, job.Status, job.Attempts, job.InstallationID != nil) != "" {
		return Job{}, ErrRetryNotAllowed
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT status='active' FROM integrations WHERE id=(SELECT integration_id FROM installations WHERE id=$1) FOR SHARE`, job.InstallationID).Scan(&active); err != nil {
		return Job{}, err
	}
	if !active {
		return Job{}, ErrRetryNotAllowed
	}
	if err := tx.QueryRow(ctx, `SELECT status='active' FROM installations WHERE id=$1 FOR SHARE`, job.InstallationID).Scan(&active); err != nil {
		return Job{}, err
	}
	if !active {
		return Job{}, ErrRetryNotAllowed
	}
	job, err = scanJob(tx.QueryRow(ctx, `UPDATE jobs SET status='retry', max_attempts=attempts+5,
		run_after=now(), finished_at=NULL, last_error_code=NULL, last_error_message=NULL,
		locked_by=NULL, locked_until=NULL, result=NULL WHERE id=$1 RETURNING `+jobColumns, id))
	if err != nil {
		return Job{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id)
		VALUES($1,'admin',$2,'job.retry','job',$3)`, job.InstallationID, actor, id.String()); err != nil {
		return Job{}, err
	}
	return job, nil
}
