package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
		SET status = 'running', attempt = attempt + 1, lease_until = clock_timestamp() + ($2::bigint * interval '1 millisecond')
		FROM next WHERE job.id = next.id
		RETURNING job.id, job.kind, job.payload, job.attempt, job.max_attempts
	`, kinds, ttl.Milliseconds()).Scan(&lease.ID, &lease.Kind, &lease.Payload, &lease.Attempt, &lease.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	return lease, err == nil, err
}

func (s Store) Finish(ctx context.Context, lease Lease, handlerErr error) (Status, error) {
	status := Succeeded
	var retryAfter time.Duration
	if handlerErr != nil {
		status = Dead
		if lease.Attempt < lease.MaxAttempts {
			status = Queued
			retryAfter = min(time.Second<<(lease.Attempt-1), time.Minute)
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE platform_jobs.jobs
		SET status = $3, lease_until = NULL,
			available_at = CASE WHEN $3 = 'queued' THEN clock_timestamp() + ($4::bigint * interval '1 millisecond') ELSE available_at END
		WHERE id = $1 AND attempt = $2 AND status = 'running' AND lease_until > clock_timestamp()
	`, lease.ID, lease.Attempt, status, retryAfter.Milliseconds())
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
				lease_until = NULL, available_at = clock_timestamp()
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
