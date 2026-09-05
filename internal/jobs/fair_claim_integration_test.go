package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func seedQueueInstallation(t *testing.T, pool *pgxpool.Pool, integration uuid.UUID, account int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)
        VALUES ($1,$2,$2,'secret','https://example.test/callback') ON CONFLICT (id) DO NOTHING`, integration, integration.String()); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO installations(integration_id,account_id,account_domain,status)
        VALUES ($1,$2,'same-account.amocrm.ru','active') RETURNING id`, integration, account).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedQueueLoad(t *testing.T, pool *pgxpool.Pool, installation uuid.UUID, count, priority int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO jobs(installation_id,type,payload,priority,run_after)
        SELECT $1,'workflow.lead.set_status','{}'::jsonb,$2,now()-interval '1 minute'
        FROM generate_series(1,$3::integer)`, installation, priority, count); err != nil {
		t.Fatal(err)
	}
}

// The older global priority selector cannot admit the small integration while
// its high-priority neighbor replenishes. This compares the real selectors on
// the same workload and measures progress in worker slots, not flaky wall time.
func TestFairClaimCapacityNoisyNeighbor(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	a := seedQueueInstallation(t, pool, uuid.New(), 42)
	b := seedQueueInstallation(t, pool, uuid.New(), 42)
	seedQueueLoad(t, pool, a, 100000, 1)
	seedQueueLoad(t, pool, b, 8, 100)
	store, ctx := NewStore(pool), context.Background()
	globalStarted := time.Now()
	for tick := range 6 {
		claimed, err := store.Claim(ctx, "legacy", 4, time.Minute)
		if err != nil || len(claimed) != 4 {
			t.Fatalf("legacy tick %d count=%d err=%v", tick, len(claimed), err)
		}
		for _, job := range claimed {
			if *job.InstallationID != a {
				t.Fatal("expected large priority backlog to occupy every legacy slot")
			}
			if err := store.Complete(ctx, job, "legacy", nil, time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
		seedQueueLoad(t, pool, a, 4, 1)
	}
	t.Logf("global claim: B progressed 0/8 after 24 occupied worker slots, elapsed=%s", time.Since(globalStarted))
	fairStarted := time.Now()
	smallCompleted := 0
	for tick := range 4 {
		claimContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		claimed, err := store.ClaimFairWithObserver(claimContext, "fair", 4, 100, time.Minute, 2, nil)
		cancel()
		if err != nil || len(claimed) != 4 {
			t.Fatalf("fair tick %d count=%d err=%v", tick, len(claimed), err)
		}
		smallThisTick := 0
		for _, job := range claimed {
			if *job.InstallationID == b {
				smallThisTick++
				smallCompleted++
			}
			if err := store.Complete(ctx, job, "fair", nil, time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
		if smallThisTick != 2 {
			t.Fatalf("tick %d small integration got %d slots, want 2", tick, smallThisTick)
		}
		seedQueueLoad(t, pool, a, 4, 1)
	}
	if smallCompleted != 8 {
		t.Fatalf("small integration completed %d/8", smallCompleted)
	}
	t.Logf("fair claim: B completed 8/8 within 16 worker slots with A continuously replenished, elapsed=%s", time.Since(fairStarted))
}

func TestFairClaimReplicaCapAggregatesInstallationsAndPlatform(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integration := uuid.New()
	a1 := seedQueueInstallation(t, pool, integration, 42)
	a2 := seedQueueInstallation(t, pool, integration, 43)
	b := seedQueueInstallation(t, pool, uuid.New(), 42)
	for _, id := range []uuid.UUID{a1, a2, b} {
		seedQueueLoad(t, pool, id, 20, 100)
	}
	store, ctx := NewStore(pool), context.Background()
	for range 8 {
		if _, err := store.Enqueue(ctx, EnqueueParams{Type: "platform.test", Payload: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	start := make(chan struct{})
	for replica := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ClaimFairWithObserver(ctx, fmt.Sprintf("replica-%d", replica), 8, 100, time.Minute, 2, nil)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT i.integration_id,count(*) FROM jobs j LEFT JOIN installations i ON i.id=j.installation_id
        WHERE j.status='processing' GROUP BY i.integration_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lanes := 0
	for rows.Next() {
		var scope *uuid.UUID
		var count int
		if err := rows.Scan(&scope, &count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("scope %v live leases=%d, want 2", scope, count)
		}
		lanes++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if lanes != 3 {
		t.Fatalf("claimed lanes=%d,want integration A,B and platform", lanes)
	}
}

func TestFairClaimRotatesSingleSlotAcrossPollsAndReapsFencedLease(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	a := seedQueueInstallation(t, pool, uuid.New(), 42)
	b := seedQueueInstallation(t, pool, uuid.New(), 42)
	seedQueueLoad(t, pool, a, 10, 1)
	seedQueueLoad(t, pool, b, 10, 100)
	store, ctx := NewStore(pool), context.Background()
	first, err := store.ClaimFairWithObserver(ctx, "worker", 1, 100, time.Minute, 1, nil)
	if err != nil || len(first) != 1 {
		t.Fatalf("first: %v %v", first, err)
	}
	if err := store.Complete(ctx, first[0], "worker", nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimFairWithObserver(ctx, "worker", 1, 100, time.Minute, 1, nil)
	if err != nil || len(second) != 1 {
		t.Fatalf("second: %v %v", second, err)
	}
	if *first[0].InstallationID == *second[0].InstallationID {
		t.Fatal("single-slot polling starved other integration")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, second[0].ID); err != nil {
		t.Fatal(err)
	}
	observed := false
	next, err := store.ClaimFairWithObserver(ctx, "new-worker", 4, 100, time.Minute, 1, func(_ context.Context, _ TxExecutor, job Job, failure Failure, status Status) error {
		observed = job.ID == second[0].ID && failure.Code == "lease_expired" && status == StatusRetry
		return nil
	})
	if err != nil || len(next) != 2 || !observed {
		t.Fatalf("reclaim count=%d observed=%v err=%v", len(next), observed, err)
	}
	if err := store.Complete(ctx, second[0], "worker", nil, time.Millisecond); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
}

func TestFairClaimRollbackOnObserverFailureAndSchedulerTimeout(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store, ctx := NewStore(pool), context.Background()
	created, err := store.Enqueue(ctx, EnqueueParams{Type: "platform.test", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimFairWithObserver(ctx, "crashed", 1, 100, time.Minute, 1, nil)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: count=%d err=%v", len(first), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	observerError := errors.New("observer transaction failed")
	if _, err := store.ClaimFairWithObserver(ctx, "replacement", 1, 100, time.Minute, 1, func(context.Context, TxExecutor, Job, Failure, Status) error { return observerError }); !errors.Is(err, observerError) {
		t.Fatalf("observer failure: %v", err)
	}
	var attempts int
	var status Status
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT attempts,status,(SELECT count(*) FROM job_attempts WHERE job_id=$1) FROM jobs WHERE id=$1`, created.ID).Scan(&attempts, &status, &recorded); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || status != StatusProcessing || recorded != 0 {
		t.Fatalf("partial observer commit: attempts=%d,status=%s,history=%d", attempts, status, recorded)
	}
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(ctx) }()
	if _, err := locker.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, fairClaimLockID); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, err := store.ClaimFairWithObserver(deadline, "blocked", 1, 100, time.Minute, 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduler ignored claim deadline: %v", err)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.ClaimFairWithObserver(ctx, "recovered", 1, 100, time.Minute, 1, nil)
	if err != nil || len(recovered) != 1 || recovered[0].Attempts != 2 {
		t.Fatalf("recovery after rollback: %v %v", recovered, err)
	}
}
