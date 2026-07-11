package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

var (
	ErrNotFound  = errors.New("job not found")
	ErrLeaseLost = errors.New("job lease lost")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Enqueue(ctx context.Context, params EnqueueParams) (Job, error) {
	return enqueue(ctx, s.pool, params)
}

// EnqueueTx inserts a job through the caller's transaction. It lets domain
// stores commit a job and their idempotency records as one PostgreSQL unit.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, params EnqueueParams) (Job, error) {
	if tx == nil {
		return Job{}, errors.New("enqueue transaction is nil")
	}
	return enqueue(ctx, tx, params)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func enqueue(ctx context.Context, querier rowQuerier, params EnqueueParams) (Job, error) {
	if err := validateEnqueue(params); err != nil {
		return Job{}, err
	}
	payload, err := json.Marshal(params.Payload)
	if err != nil {
		return Job{}, fmt.Errorf("marshal job payload: %w", err)
	}
	if params.Priority == 0 {
		params.Priority = 100
	}
	if params.MaxAttempts == 0 {
		params.MaxAttempts = 5
	}
	if params.RunAfter.IsZero() {
		params.RunAfter = time.Now().UTC()
	}

	row := querier.QueryRow(ctx, `
		INSERT INTO jobs (installation_id, type, priority, payload, max_attempts, run_after)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+jobColumns,
		params.InstallationID, params.Type, params.Priority, payload, params.MaxAttempts, params.RunAfter,
	)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, fmt.Errorf("enqueue job: %w", err)
	}
	return job, nil
}

func (s *Store) Claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]Job, error) {
	return s.ClaimWithObserver(ctx, workerID, limit, lease, nil)
}

func (s *Store) ClaimWithObserver(
	ctx context.Context,
	workerID string,
	limit int,
	lease time.Duration,
	observer FailureObserver,
) ([]Job, error) {
	if workerID == "" || limit < 1 || lease < time.Millisecond {
		return nil, errors.New("worker id, positive limit, and lease of at least 1ms are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim jobs: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	reapLimit := max(limit*4, 100)
	if err := reapExpired(ctx, tx, reapLimit, observer); err != nil {
		return nil, err
	}
	if err := reapExhausted(ctx, tx, reapLimit, observer); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		WITH selected AS (
			SELECT id
			FROM jobs
			WHERE status IN ('queued', 'retry') AND run_after <= now()
			  AND attempts < max_attempts
			ORDER BY priority ASC, run_after ASC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE jobs AS j
		SET status = 'processing',
			locked_by = $2,
			locked_until = now() + ($3 * interval '1 millisecond'),
			attempts = attempts + 1,
			updated_at = now()
		FROM selected
		WHERE j.id = selected.id
		RETURNING `+prefixedJobColumns("j"),
		limit, workerID, lease.Milliseconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()

	claimed := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed job: %w", err)
		}
		claimed = append(claimed, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed jobs: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claimed jobs: %w", err)
	}
	return claimed, nil
}

func reapExpired(ctx context.Context, tx pgx.Tx, limit int, observer FailureObserver) error {
	rows, err := tx.Query(ctx, `
		SELECT `+jobColumns+`
		FROM jobs
		WHERE status = 'processing' AND locked_until < now()
		ORDER BY locked_until ASC, created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("select expired job leases: %w", err)
	}
	expired := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("scan expired job lease: %w", err)
		}
		expired = append(expired, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate expired job leases: %w", err)
	}
	rows.Close()

	for _, job := range expired {
		workerID := "expired-worker"
		if job.LockedBy != nil {
			workerID = *job.LockedBy
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_attempts (
				job_id, attempt, worker_id, started_at, finished_at,
				outcome, error_code, error_message, duration_ms
			) VALUES (
				$1, $2, $3, $4, now(), 'lease_expired',
				'lease_expired', 'worker lease expired',
				GREATEST(0, EXTRACT(EPOCH FROM (now() - $4::timestamptz)) * 1000)::bigint
			) ON CONFLICT (job_id, attempt) DO NOTHING`,
			job.ID, job.Attempts, workerID, job.UpdatedAt,
		); err != nil {
			return fmt.Errorf("record expired job attempt: %w", err)
		}
		status := StatusRetry
		if job.Attempts >= job.MaxAttempts {
			status = StatusDead
		}
		tag, err := tx.Exec(ctx, `
			UPDATE jobs
			SET status = $3, run_after = now(), locked_by = NULL, locked_until = NULL,
				last_error_code = 'lease_expired', last_error_message = 'worker lease expired',
				finished_at = CASE WHEN $3 = 'dead' THEN now() ELSE NULL END,
				updated_at = now()
			WHERE id = $1 AND status = 'processing' AND attempts = $2`,
			job.ID, job.Attempts, status,
		)
		if err != nil {
			return fmt.Errorf("reap expired job: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		if observer != nil {
			failure := Failure{Code: "lease_expired", Message: "worker lease expired", Retryable: status == StatusRetry}
			if err := observer(ctx, tx, job, failure, status); err != nil {
				return fmt.Errorf("observe expired job %s: %w", job.ID, err)
			}
		}
	}
	return nil
}

func reapExhausted(ctx context.Context, tx pgx.Tx, limit int, observer FailureObserver) error {
	rows, err := tx.Query(ctx, `
		SELECT `+jobColumns+`
		FROM jobs
		WHERE status IN ('queued', 'retry') AND attempts >= max_attempts
		ORDER BY run_after ASC, created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("select exhausted jobs: %w", err)
	}
	exhausted := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("scan exhausted job: %w", err)
		}
		exhausted = append(exhausted, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate exhausted jobs: %w", err)
	}
	rows.Close()

	for _, job := range exhausted {
		code, message := "attempts_exhausted", "job attempts exhausted"
		if job.LastErrorCode != nil {
			code = *job.LastErrorCode
		}
		if job.LastErrorMessage != nil {
			message = *job.LastErrorMessage
		}
		if _, err := tx.Exec(ctx, `
			UPDATE jobs
			SET status = 'dead', finished_at = now(), updated_at = now(),
				last_error_code = $2, last_error_message = $3
			WHERE id = $1 AND status IN ('queued', 'retry') AND attempts >= max_attempts`,
			job.ID, sanitize.Text(code, 200), sanitize.Text(message, 4000),
		); err != nil {
			return fmt.Errorf("reap exhausted job: %w", err)
		}
		if observer != nil {
			failure := Failure{Code: code, Message: message, Retryable: false}
			if err := observer(ctx, tx, job, failure, StatusDead); err != nil {
				return fmt.Errorf("observe exhausted job %s: %w", job.ID, err)
			}
		}
	}
	return nil
}

func (s *Store) ExtendLease(ctx context.Context, id uuid.UUID, workerID string, attempt int, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET locked_until = now() + ($4 * interval '1 millisecond'), updated_at = now()
		WHERE id = $1 AND status = 'processing' AND locked_by = $2
		  AND attempts = $3 AND locked_until >= now()`,
		id, workerID, attempt, lease.Milliseconds(),
	)
	if err != nil {
		return fmt.Errorf("extend job lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Complete(ctx context.Context, job Job, workerID string, result json.RawMessage, duration time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = 'completed', result = $4, locked_by = NULL, locked_until = NULL,
			last_error_code = NULL, last_error_message = NULL,
			finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'processing' AND locked_by = $2
		  AND attempts = $3 AND locked_until >= now()`,
		job.ID, workerID, job.Attempts, nullableJSON(result),
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if err := insertAttempt(ctx, tx, job, workerID, "completed", "", "", duration); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Fail(ctx context.Context, job Job, workerID string, failure Failure, duration time.Duration) (Status, error) {
	return s.FailWithObserver(ctx, job, workerID, failure, duration, nil)
}

func (s *Store) FailWithObserver(
	ctx context.Context,
	job Job,
	workerID string,
	failure Failure,
	duration time.Duration,
	observer FailureObserver,
) (Status, error) {
	status := StatusFailed
	finished := true
	if failure.Retryable {
		if job.Attempts >= job.MaxAttempts {
			status = StatusDead
		} else {
			status = StatusRetry
			finished = false
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin fail job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = $4,
			run_after = now() + ($5 * interval '1 millisecond'),
			locked_by = NULL, locked_until = NULL,
			last_error_code = $6, last_error_message = $7,
			finished_at = CASE WHEN $8 THEN now() ELSE NULL END, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND locked_by = $2
		  AND attempts = $3 AND locked_until >= now()`,
		job.ID, workerID, job.Attempts, status, failure.RetryAfter.Milliseconds(), sanitize.Text(failure.Code, 200), sanitize.Text(failure.Message, 4000), finished,
	)
	if err != nil {
		return "", fmt.Errorf("fail job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return "", ErrLeaseLost
	}
	if err := insertAttempt(ctx, tx, job, workerID, string(status), failure.Code, failure.Message, duration); err != nil {
		return "", err
	}
	if observer != nil {
		if err := observer(ctx, tx, job, failure, status); err != nil {
			return "", fmt.Errorf("observe job failure: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit failed job: %w", err)
	}
	return status, nil
}

func (s *Store) GetForInstallation(ctx context.Context, id, installationID uuid.UUID) (Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, `
		SELECT `+jobColumns+` FROM jobs WHERE id = $1 AND installation_id = $2`, id, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

const jobColumns = `
	id, installation_id, type, status, priority, payload, result,
	attempts, max_attempts, run_after, locked_by, locked_until,
	last_error_code, last_error_message, created_at, updated_at, finished_at`

func prefixedJobColumns(prefix string) string {
	return prefix + ".id, " + prefix + ".installation_id, " + prefix + ".type, " + prefix + ".status, " +
		prefix + ".priority, " + prefix + ".payload, " + prefix + ".result, " + prefix + ".attempts, " +
		prefix + ".max_attempts, " + prefix + ".run_after, " + prefix + ".locked_by, " + prefix +
		".locked_until, " + prefix + ".last_error_code, " + prefix + ".last_error_message, " +
		prefix + ".created_at, " + prefix + ".updated_at, " + prefix + ".finished_at"
}

type scanner interface {
	Scan(...any) error
}

func scanJob(row scanner) (Job, error) {
	var job Job
	err := row.Scan(
		&job.ID, &job.InstallationID, &job.Type, &job.Status, &job.Priority, &job.Payload, &job.Result,
		&job.Attempts, &job.MaxAttempts, &job.RunAfter, &job.LockedBy, &job.LockedUntil,
		&job.LastErrorCode, &job.LastErrorMessage, &job.CreatedAt, &job.UpdatedAt, &job.FinishedAt,
	)
	return job, err
}

func insertAttempt(ctx context.Context, tx pgx.Tx, job Job, workerID, outcome, code, message string, duration time.Duration) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO job_attempts (
			job_id, attempt, worker_id, started_at, finished_at, outcome,
			error_code, error_message, duration_ms
		) VALUES (
			$1, $2, $3,
			now() - ($8 * interval '1 millisecond'), now(),
			$4, NULLIF($5, ''), NULLIF($6, ''), $7
		)`,
		job.ID, job.Attempts, workerID, outcome, sanitize.Text(code, 200), sanitize.Text(message, 4000), duration.Milliseconds(), duration.Milliseconds(),
	)
	if err != nil {
		return fmt.Errorf("record job attempt: %w", err)
	}
	return nil
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
