package queue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestQueueRuntime(t *testing.T) {
	dsn := os.Getenv("PLATFORM_TEST_OWNER_DSN")
	if dsn == "" {
		t.Skip("run make platform-test for Platform queue checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	access, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_ACCESS_DSN"), storage.Access, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()

	t.Run("dedicated roles and pools", func(t *testing.T) {
		if err := storage.CheckAccess(ctx, access); err != nil {
			t.Fatal(err)
		}
		var readOnly string
		if err := access.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
			t.Fatalf("access transactions read-only = %q, %v", readOnly, err)
		}

		if _, err := access.Exec(ctx, "SELECT * FROM platform_jobs.jobs"); !hasSQLState(err, "42501") {
			t.Fatalf("access role queue query error = %v", err)
		}
		if _, err := jobs.Exec(ctx, "SELECT * FROM platform_access.runtime_probe"); !hasSQLState(err, "42501") {
			t.Fatalf("jobs role access query error = %v", err)
		}
		for _, query := range []string{
			"UPDATE platform_jobs.jobs SET payload = '{}'::jsonb",
			"DELETE FROM platform_jobs.jobs",
			"CREATE TABLE platform_jobs.foreign_table (id bigint)",
			"SELECT * FROM public.schema_migrations",
		} {
			if _, err := jobs.Exec(ctx, query); !hasSQLState(err, "42501") {
				t.Fatalf("jobs role query %q returned %v", query, err)
			}
		}
		check, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		if pool, err := storage.Open(check, dsn, storage.Access, 1); err == nil {
			pool.Close()
			t.Fatal("runtime accepted the Platform owner DSN")
		}
		held, err := jobs.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		check, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		if err := jobs.Ping(check); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("jobs pool saturation check: %v", err)
		}
		if err := storage.CheckAccess(ctx, access); err != nil {
			t.Fatal(err)
		}
	})

	store := NewStore(jobs)
	t.Run("concurrent claim recovery and fencing", func(t *testing.T) {
		kind := Kind("lease_fixture")
		jobID := seedJob(ctx, t, owner, kind, 2)
		secondPool, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer secondPool.Close()
		var group sync.WaitGroup
		type result struct {
			lease Lease
			found bool
			err   error
		}
		claims := make(chan result, 2)
		for _, candidate := range []Store{store, NewStore(secondPool)} {
			group.Go(func() {
				lease, found, err := candidate.Claim(ctx, []Kind{kind}, 45*time.Second)
				claims <- result{lease, found, err}
			})
		}
		group.Wait()
		close(claims)
		var claimed []Lease
		for res := range claims {
			if res.err != nil {
				t.Fatal(res.err)
			}
			if res.found {
				claimed = append(claimed, res.lease)
			}
		}
		if len(claimed) != 1 || claimed[0].ID != jobID || claimed[0].Attempt != 1 {
			t.Fatalf("expected one initial claim, got %v", claimed)
		}
		expireLease(ctx, t, owner, jobID)
		if _, err := store.Finish(ctx, claimed[0], nil); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("completion after expiration returned %v", err)
		}
		if retried, dead, err := store.Recover(ctx); err != nil || retried != 1 || dead != 0 {
			t.Fatalf("recover from crash: %d retried, %d dead, %v", retried, dead, err)
		}
		newLease, found, err := store.Claim(ctx, []Kind{kind}, 45*time.Second)
		if err != nil || !found || newLease.ID != jobID || newLease.Attempt != 2 {
			t.Fatalf("recover claim: %v, %v, %v", newLease, found, err)
		}
		if _, err := store.Finish(ctx, claimed[0], nil); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("old lease completed a new attempt: %v", err)
		}
		expireLease(ctx, t, owner, jobID)
		if retried, dead, err := store.Recover(ctx); err != nil || retried != 0 || dead != 1 {
			t.Fatalf("recover final expiration: %d retried, %d dead, %v", retried, dead, err)
		}
		assertState(ctx, t, owner, jobID, Dead, 2)
	})

	t.Run("handler registry and metrics", func(t *testing.T) {
		kind := Kind("handler_fixture")
		jobID := seedJob(ctx, t, owner, kind, 2)
		unknown := seedJob(ctx, t, owner, "unregistered_fixture", 2)
		crashed := seedJob(ctx, t, owner, "previously_registered_fixture", 1)
		if _, found, err := store.Claim(ctx, []Kind{"previously_registered_fixture"}, 45*time.Second); err != nil || !found {
			t.Fatalf("claim crash fixture: %t, %v", found, err)
		}
		expireLease(ctx, t, owner, crashed)

		calls := make(chan Job, 2)
		var attempts atomic.Int32
		failure := errors.New("fixture retry")
		reg := prometheus.NewRegistry()
		cfg := DefaultConfig(2)
		cfg.PollInterval = 10 * time.Millisecond
		cfg.SweepInterval = 20 * time.Millisecond
		runtime := NewWorker(store, map[Kind]Handler{
			kind: func(_ context.Context, job Job) error {
				attempt := attempts.Add(1)
				calls <- job
				if attempt == 1 {
					return failure
				}
				return nil
			},
		}, cfg, reg)
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			runtime.Run(workerCtx)
		}()
		defer func() {
			stop()
			<-done
		}()

		for attempt := 1; attempt <= 2; attempt++ {
			select {
			case job := <-calls:
				if job.ID != jobID || !json.Valid(job.Payload) {
					t.Fatalf("handler received unexpected job: %+v", job)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		awaitMetricValue(ctx, t, reg, "platform_queue_outcomes_total", "outcome", "queued", 1)
		awaitMetricValue(ctx, t, reg, "platform_queue_outcomes_total", "outcome", "succeeded", 1)
		awaitMetricValue(ctx, t, reg, "platform_queue_outcomes_total", "outcome", "recovered_dead", 1)
		awaitMetricValue(ctx, t, reg, "platform_queue_depth", "status", "queued", 1)
		assertState(ctx, t, owner, jobID, Succeeded, 2)
		assertState(ctx, t, owner, unknown, Queued, 0)
		assertState(ctx, t, owner, crashed, Dead, 1)
	})
}

func hasSQLState(err error, code string) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == code
}

func seedJob(ctx context.Context, t *testing.T, owner *pgxpool.Pool, kind Kind, maxAttempts int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := owner.Exec(ctx, "INSERT INTO platform_jobs.jobs (id, kind, payload, max_attempts) VALUES ($1, $2, '{}', $3)", id, kind, maxAttempts); err != nil {
		t.Fatal(err)
	}
	return id
}

func expireLease(ctx context.Context, t *testing.T, owner *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := owner.Exec(ctx, "UPDATE platform_jobs.jobs SET lease_until = statement_timestamp() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
}

func assertState(ctx context.Context, t *testing.T, owner *pgxpool.Pool, id uuid.UUID, status Status, attempt int) {
	t.Helper()
	var state Status
	var attempts int
	if err := owner.QueryRow(ctx, "SELECT status, attempt FROM platform_jobs.jobs WHERE id = $1", id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != status || attempts != attempt {
		t.Fatalf("queue job status %s at attempt %d, want %s at %d", state, attempts, status, attempt)
	}
}

func awaitMetricValue(ctx context.Context, t *testing.T, registry *prometheus.Registry, name, key, value string, want float64) {
	t.Helper()
	for ctx.Err() == nil {
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() != name {
				continue
			}
			for _, metric := range family.GetMetric() {
				labels := metric.GetLabel()
				if len(labels) != 1 || labels[0].GetName() != key || labels[0].GetValue() != value {
					continue
				}
				if metric.GetCounter().GetValue()+metric.GetGauge().GetValue() == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metric %s{%s=%s} did not reach %v", name, key, value, want)
}
