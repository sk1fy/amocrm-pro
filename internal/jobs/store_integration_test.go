package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestStoreRecoversLeaseAndFencesStaleAttempt(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewStore(pool)
	ctx := context.Background()
	created, err := store.Enqueue(ctx, EnqueueParams{Type: "test", Payload: map[string]any{}, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, "same-worker-id", 1, 40*time.Millisecond)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: jobs=%d err=%v", len(first), err)
	}
	time.Sleep(60 * time.Millisecond)
	second, err := store.Claim(ctx, "same-worker-id", 1, time.Second)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: jobs=%d err=%v", len(second), err)
	}
	if second[0].Attempts != 2 {
		t.Fatalf("expected attempt 2, got %d", second[0].Attempts)
	}
	if err := store.Complete(ctx, first[0], "same-worker-id", json.RawMessage(`{}`), time.Millisecond); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale attempt should lose lease, got %v", err)
	}
	if err := store.Complete(ctx, second[0], "same-worker-id", json.RawMessage(`{"ok":true}`), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT outcome FROM job_attempts WHERE job_id=$1 AND attempt=1`, created.ID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "lease_expired" {
		t.Fatalf("unexpected expired attempt outcome: %s", outcome)
	}
}

func TestStoreMovesExpiredFinalAttemptToDead(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewStore(pool)
	ctx := context.Background()
	created, err := store.Enqueue(ctx, EnqueueParams{Type: "test", Payload: map[string]any{}, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "crashed-worker", 1, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	claimed, err := store.Claim(ctx, "reaper", 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("dead job was claimed: %#v", claimed)
	}
	var status Status
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusDead {
		t.Fatalf("expected dead, got %s", status)
	}
}

func TestStoreSanitizesFailureAndObserverIsAtomic(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewStore(pool)
	ctx := context.Background()
	created, err := store.Enqueue(ctx, EnqueueParams{Type: "test", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, "worker", 1, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%d err=%v", len(claimed), err)
	}
	observerError := errors.New("observer unavailable")
	_, err = store.FailWithObserver(ctx, claimed[0], "worker", Failure{Code: "bad", Message: "bad"}, time.Millisecond,
		func(context.Context, TxExecutor, Job, Failure, Status) error { return observerError })
	if !errors.Is(err, observerError) {
		t.Fatalf("expected observer error, got %v", err)
	}
	var status Status
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusProcessing {
		t.Fatalf("failure transaction was not rolled back: %s", status)
	}

	message := strings.Repeat("a", 3999) + "€\x00tail"
	if _, err := store.Fail(ctx, claimed[0], "worker", Failure{Code: "invalid", Message: message}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT last_error_message FROM jobs WHERE id=$1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored) || strings.ContainsRune(stored, '\x00') || len(stored) > 4000 {
		t.Fatalf("unsafe stored message: valid=%t nul=%t len=%d", utf8.ValidString(stored), strings.ContainsRune(stored, '\x00'), len(stored))
	}
}
