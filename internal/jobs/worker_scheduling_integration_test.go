package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestWorkerRefillsReleasedSlotBeforeFallbackPoll(t *testing.T) {
	for _, integrationCap := range []int{0, 1} {
		t.Run(fmt.Sprintf("integration_cap_%d", integrationCap), func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			store := NewStore(pool)
			for range 2 {
				if _, err := store.Enqueue(context.Background(), EnqueueParams{Type: "worker.test", Payload: map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			started, release, handler := workerBlockingHandler()
			config := workerSchedulingConfig()
			config.IntegrationConcurrency = integrationCap
			stop := startSchedulingWorker(t, store, config, handler)
			first := awaitWorkerJob(t, started)
			release <- struct{}{}
			second := awaitWorkerJob(t, started)
			if second.ID == first.ID {
				t.Fatal("worker reclaimed the completed job")
			}
			var status Status
			if err := pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id=$1`, first.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != StatusCompleted {
				t.Fatalf("slot reused before durable completion: status=%s", status)
			}
			stop()
		})
	}
}

func TestWorkerFillsCapacityAcrossClaimBatchesAndHonorsCap(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integrationA, integrationB := uuid.New(), uuid.New()
	a1 := seedQueueInstallation(t, pool, integrationA, 1)
	a2 := seedQueueInstallation(t, pool, integrationA, 2)
	b := seedQueueInstallation(t, pool, integrationB, 1)
	for _, installation := range []uuid.UUID{a1, a2, b} {
		seedQueueLoad(t, pool, installation, 5, 100)
	}
	store, trace := tracedSchedulingStore(t, pool)
	started, release, handler := workerBlockingHandler()
	config := workerSchedulingConfig()
	config.Concurrency = 6
	config.IntegrationConcurrency = 2
	startSchedulingWorker(t, store, config, handler)
	counts := map[uuid.UUID]int{}
	for range 4 {
		job := awaitWorkerJob(t, started)
		if *job.InstallationID == b {
			counts[integrationB]++
		} else {
			counts[integrationA]++
		}
	}
	if counts[integrationA] != 2 || counts[integrationB] != 2 {
		t.Fatalf("initial slots by integration=%v, want two each", counts)
	}
	awaitWorkerClaims(t, trace, 5) // Four full batches followed by one capped claim.
	assertWorkerClaimCountStable(t, trace, 5)
	select {
	case job := <-started:
		t.Fatalf("worker exceeded the shared integration cap: %+v", job)
	default:
	}
	release <- struct{}{}
	awaitWorkerJob(t, started)
	var exceeded bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT i.integration_id FROM jobs j JOIN installations i ON i.id=j.installation_id
			WHERE j.status='processing' AND j.locked_until >= statement_timestamp()
			GROUP BY i.integration_id HAVING count(*) > 2
		)`).Scan(&exceeded); err != nil {
		t.Fatal(err)
	}
	if exceeded {
		t.Fatal("completion wakeup exceeded integration cap")
	}
}

func TestWorkerStopsRefillAfterPartialClaim(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	installation := seedQueueInstallation(t, pool, uuid.New(), 1)
	seedQueueLoad(t, pool, installation, 10, 100)
	store, trace := tracedSchedulingStore(t, pool)
	started, _, handler := workerBlockingHandler()
	config := workerSchedulingConfig()
	config.Concurrency, config.BatchSize, config.IntegrationConcurrency = 8, 8, 2
	startSchedulingWorker(t, store, config, handler)
	for range 2 {
		awaitWorkerJob(t, started)
	}
	assertWorkerClaimCountStable(t, trace, 1)
}

func TestWorkerEmptyQueueUsesFallbackWithoutBusyPolling(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store, trace := tracedSchedulingStore(t, pool)
	config := workerSchedulingConfig()
	config.PollInterval = 500 * time.Millisecond
	startSchedulingWorker(t, store, config, nil)
	awaitWorkerClaims(t, trace, 1)
	assertWorkerClaimCountStable(t, trace, 1)
	awaitWorkerClaims(t, trace, 2)
}

func TestWorkerClaimTimeoutDoesNotBusyPollAndCancellationStopsWait(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	locker, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, fairClaimLockID); err != nil {
		t.Fatal(err)
	}
	store, trace := tracedSchedulingStore(t, pool)
	config := workerSchedulingConfig()
	config.ClaimTimeout = 40 * time.Millisecond
	stop := startSchedulingWorker(t, store, config, nil)
	awaitWorkerClaims(t, trace, 1)
	assertWorkerClaimCountStable(t, trace, 1)
	stop()
}

func TestWorkerCancellationDrainsActiveHandlerWithoutClaimingAgain(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewStore(pool)
	for range 2 {
		if _, err := store.Enqueue(context.Background(), EnqueueParams{Type: "worker.test", Payload: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}
	started, _, handler := workerBlockingHandler()
	stop := startSchedulingWorker(t, store, workerSchedulingConfig(), handler)
	awaitWorkerJob(t, started)
	stop()
	var processing, completed, queued int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE status='processing'), count(*) FILTER (WHERE status='completed'),
		       count(*) FILTER (WHERE status='queued') FROM jobs`).Scan(&processing, &completed, &queued); err != nil {
		t.Fatal(err)
	}
	if processing != 0 || completed != 1 || queued != 1 {
		t.Fatalf("after cancellation: processing=%d completed=%d queued=%d", processing, completed, queued)
	}
}

func workerSchedulingConfig() WorkerConfig {
	return WorkerConfig{
		ID: "scheduling-test", PollInterval: 10 * time.Second, LeaseDuration: time.Minute,
		JobTimeout: time.Minute, BatchSize: 1, ReapBatchSize: 10, Concurrency: 1,
		IntegrationConcurrency: 1, DrainTimeout: 2 * time.Second, ClaimTimeout: 2 * time.Second,
	}
}

func workerBlockingHandler() (chan Job, chan struct{}, Handler) {
	started, release := make(chan Job, 32), make(chan struct{}, 1)
	return started, release, func(ctx context.Context, job Job) (json.RawMessage, error) {
		started <- job
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{}`), nil
	}
}

func startSchedulingWorker(t *testing.T, store *Store, config WorkerConfig, handler Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	worker := NewWorker(store, slog.New(slog.NewTextHandler(io.Discard, nil)), config,
		map[string]Handler{"worker.test": handler, "workflow.lead.set_status": handler})
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop and drain before fallback poll")
		}
	}
	t.Cleanup(stop)
	return stop
}

func awaitWorkerJob(t *testing.T, started <-chan Job) Job {
	t.Helper()
	select {
	case job := <-started:
		return job
	case <-time.After(2 * time.Second):
		t.Fatal("worker left an eligible slot idle until the 10-second fallback poll")
		return Job{}
	}
}

type workerClaimTrace struct {
	claims atomic.Int64
}

func (trace *workerClaimTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(data.SQL), "begin") {
		trace.claims.Add(1)
	}
	return ctx
}

func (*workerClaimTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func tracedSchedulingStore(t *testing.T, pool *pgxpool.Pool) (*Store, *workerClaimTrace) {
	t.Helper()
	trace := &workerClaimTrace{}
	config := pool.Config()
	config.ConnConfig.Tracer = trace
	tracedPool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracedPool.Close)
	return NewStore(tracedPool), trace
}

func awaitWorkerClaims(t *testing.T, trace *workerClaimTrace, want int64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for trace.claims.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("worker claim attempts=%d, want at least %d", trace.claims.Load(), want)
		case <-ticker.C:
		}
	}
}

func assertWorkerClaimCountStable(t *testing.T, trace *workerClaimTrace, want int64) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := trace.claims.Load(); got != want {
		t.Fatalf("worker retried without a release or fallback tick: claim attempts=%d, want %d", got, want)
	}
}
