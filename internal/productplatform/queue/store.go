package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Kind string

type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Dead      Status = "dead"
)

type Job struct {
	ID      uuid.UUID
	Kind    Kind
	Payload json.RawMessage
}

type Lease struct {
	Job
	Attempt     int
	MaxAttempts int
}

var ErrLeaseLost = errors.New("job lease lost")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) Store {
	return Store{pool: pool}
}

func (s Store) Claim(ctx context.Context, kinds []Kind, ttl time.Duration) (Lease, bool, error) {
	var lease Lease
	err := s.pool.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM platform_jobs.jobs
			WHERE status = 'queued' AND available_at <= statement_timestamp() AND kind = ANY($1)
			ORDER BY available_at, id
			LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE platform_jobs.jobs AS job
		SET status = 'running', attempt = attempt + 1, updated_at = clock_timestamp(), lease_until = clock_timestamp() + ($2::bigint * interval '1 millisecond')
		FROM next WHERE job.id = next.id
		RETURNING job.id, job.kind, job.payload, job.attempt, job.max_attempts
	`, kinds, ttl.Milliseconds()).Scan(&lease.ID, &lease.Kind, &lease.Payload, &lease.Attempt, &lease.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	return lease, err == nil, err
}

type Outcome struct {
	FailureCode string
	Retry       bool
	RetryAfter  time.Duration
}

type Completion struct {
	Outcome Outcome
	// Publish runs after the lease is locked. The returned outcome and its
	// database effects are committed together or discarded together.
	Publish func(context.Context, pgx.Tx) (Outcome, error)
}

func legacyOutcome(err error) Outcome {
	if err != nil {
		return Outcome{FailureCode: "job_failed", Retry: true}
	}
	return Outcome{}
}

func (s Store) Finish(ctx context.Context, lease Lease, handlerErr error) (Status, error) {
	return s.finish(ctx, lease, Completion{Outcome: legacyOutcome(handlerErr)})
}

func (s Store) finish(ctx context.Context, lease Lease, completion Completion) (Status, error) {
	if completion.Publish == nil {
		return finishLease(ctx, s.pool, lease, completion.Outcome)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM platform_jobs.jobs
		WHERE id=$1 AND attempt=$2 AND status='running' AND lease_until>clock_timestamp()
		FOR UPDATE`, lease.ID, lease.Attempt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", err
	}
	outcome, err := completion.Publish(ctx, tx)
	if err != nil {
		return "", err
	}
	status, err := finishLease(ctx, tx, lease, outcome)
	if err != nil {
		return "", err
	}
	return status, tx.Commit(ctx)
}

type leaseExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func finishLease(ctx context.Context, db leaseExecutor, lease Lease, outcome Outcome) (Status, error) {
	status := Succeeded
	var retryAfter time.Duration
	if outcome.FailureCode != "" {
		status = Dead
		retryAfter = outcome.RetryAfter
		if outcome.Retry && lease.Attempt < lease.MaxAttempts {
			status = Queued
			retryAfter = max(min(time.Second<<(lease.Attempt-1), time.Minute), outcome.RetryAfter)
		}
	}
	delayMilliseconds := retryAfter.Milliseconds()
	if retryAfter%time.Millisecond > 0 {
		delayMilliseconds++
	}
	tag, err := db.Exec(ctx, `
		UPDATE platform_jobs.jobs
		SET status=$3, lease_until=NULL, updated_at=clock_timestamp(), failure_code=NULLIF($5,''),
			available_at=CASE WHEN $4::bigint>0 THEN clock_timestamp()+($4*interval '1 millisecond') ELSE available_at END
		WHERE id=$1 AND attempt=$2 AND status='running' AND lease_until>clock_timestamp()
	`, lease.ID, lease.Attempt, status, delayMilliseconds, outcome.FailureCode)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", ErrLeaseLost
	}
	return status, nil
}

func (s Store) Recover(ctx context.Context) (retried, dead int64, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH expired AS (
			SELECT id FROM platform_jobs.jobs
			WHERE status = 'running' AND lease_until <= statement_timestamp()
			ORDER BY lease_until, id
			LIMIT 100 FOR UPDATE SKIP LOCKED
		), recovered AS (
			UPDATE platform_jobs.jobs AS job
			SET status = CASE WHEN attempt = max_attempts THEN 'dead' ELSE 'queued' END,
				lease_until = NULL, available_at = clock_timestamp(), updated_at = clock_timestamp(), failure_code = 'lease_expired'
			FROM expired WHERE job.id = expired.id
			RETURNING job.status
		)
		SELECT count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'dead') FROM recovered
	`).Scan(&retried, &dead)
	return retried, dead, err
}

type stats struct {
	queued        int64
	running       int64
	expired       int64
	oldestSeconds float64
}

func (s Store) stats(ctx context.Context) (stats, error) {
	var state stats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'queued'),
		       count(*) FILTER (WHERE status = 'running'),
		       count(*) FILTER (WHERE status = 'running' AND lease_until <= statement_timestamp()),
		       COALESCE(GREATEST(EXTRACT(EPOCH FROM statement_timestamp() - min(available_at) FILTER (WHERE status = 'queued')), 0), 0)
		FROM platform_jobs.jobs WHERE status IN ('queued', 'running')
	`).Scan(&state.queued, &state.running, &state.expired, &state.oldestSeconds)
	return state, err
}
