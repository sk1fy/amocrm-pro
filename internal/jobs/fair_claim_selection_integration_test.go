package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestFairClaimOrdersReadyJobsAcrossInstallationsAndSkipsLocked(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	store := NewStore(pool)
	integration := uuid.New()
	a := seedQueueInstallation(t, pool, integration, 1)
	b := seedQueueInstallation(t, pool, integration, 2)
	add := func(installation uuid.UUID, priority int16, after time.Time) Job {
		t.Helper()
		job, err := store.Enqueue(ctx, EnqueueParams{
			InstallationID: &installation, Type: "test", Payload: map[string]any{},
			Priority: priority, RunAfter: after,
		})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	due := time.Now().Add(-time.Minute)
	future := add(a, 1, time.Now().Add(time.Hour))
	locked := add(a, 2, due)
	second := add(a, 4, due)
	first := add(b, 3, due)
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(ctx) }()
	if _, err := locker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, locked.ID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	claimed, err := store.ClaimFairWithObserver(claimCtx, "worker", 4, 100, time.Minute, 4, nil)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ready claim count=%d err=%v", len(claimed), err)
	}
	if claimed[0].ID != first.ID || claimed[1].ID != second.ID {
		t.Fatalf("priority across installations: got %v, %v; want %v, %v", claimed[0].ID, claimed[1].ID, first.ID, second.ID)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET run_after=statement_timestamp()-interval '1 second' WHERE id=$1`, future.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimFairWithObserver(claimCtx, "worker", 4, 100, time.Minute, 4, nil)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("newly ready claim count=%d err=%v", len(claimed), err)
	}
	if claimed[0].ID != future.ID || claimed[1].ID != locked.ID {
		t.Fatalf("newly ready and unlocked jobs: got %v, %v", claimed[0].ID, claimed[1].ID)
	}
}

func TestFairClaimIgnoresExpiredLeasesBeyondReapLimit(t *testing.T) {
	for _, platform := range []bool{false, true} {
		name := "integration"
		if platform {
			name = "platform"
		}
		t.Run(name, func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			ctx := context.Background()
			var installation *uuid.UUID
			if !platform {
				id := seedQueueInstallation(t, pool, uuid.New(), 1)
				installation = &id
			}
			if _, err := pool.Exec(ctx, `INSERT INTO jobs(installation_id,type,status,attempts,locked_by,locked_until)
				SELECT $1,'test','processing',1,'expired',statement_timestamp()-interval '1 minute'
				FROM generate_series(1,3)`, installation); err != nil {
				t.Fatal(err)
			}
			store := NewStore(pool)
			ready, err := store.Enqueue(ctx, EnqueueParams{InstallationID: installation, Type: "test", Payload: map[string]any{}, Priority: 1})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimFairWithObserver(ctx, "worker", 4, 1, time.Minute, 1, nil)
			if err != nil || len(claimed) != 1 || claimed[0].ID != ready.ID {
				t.Fatalf("expired leases consumed capacity: claimed=%v err=%v", claimed, err)
			}
			var expired, live int
			if err := pool.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE locked_until < statement_timestamp()),
				count(*) FILTER (WHERE locked_until >= statement_timestamp())
				FROM jobs WHERE status='processing'`).Scan(&expired, &live); err != nil {
				t.Fatal(err)
			}
			if expired != 2 || live != 1 {
				t.Fatalf("expired=%d live=%d, want 2 and 1", expired, live)
			}
		})
	}
}
