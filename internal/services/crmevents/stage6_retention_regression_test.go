package crmevents

import (
	"context"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestTechnicalHistoryReplayAfterGCDoesNotRollbackNewerSettings(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "without_gc"
		if cleanup {
			name = "with_gc"
		}
		t.Run(name, func(t *testing.T) { technicalHistoryReplay(t, cleanup) })
	}
}

func technicalHistoryReplay(t *testing.T, cleanup bool) {
	s, p, _ := setup(t)
	store := testStore(s)
	ctx := context.Background()
	accepted(t, s, p)
	runPages(t, s, 2)
	for _, query := range []string{
		`UPDATE event_jobs SET status='completed',updated_at=now()-interval '8 days'`,
		`UPDATE event_operations SET status='completed',updated_at=now()-interval '8 days'`,
		`UPDATE event_inbox SET created_at=now()-interval '8 days'`,
	} {
		if _, err := store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if cleanup {
		if _, err := store.Retain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Apply(ctx, serviceapi.Command{CommandID: "new-settings", Kind: "sync", InitialDays: 3, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	// After the Core calendar horizon, a late deliver of the collected command
	// must not apply as a new accept and roll back newer settings.
	_, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if cleanup {
		if serviceapi.ErrorCode(err) != serviceapi.Conflict {
			t.Fatalf("late replay after receipt GC: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	var days int
	if err := store.pool.QueryRow(ctx, `SELECT retention_days FROM event_sources WHERE installation_id=$1`, p.principal.InstallationID).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days != 30 {
		t.Fatalf("old redelivery after GC rolled back newer retention: got %d, want 30", days)
	}
}

func TestTechnicalHistoryRetainsPausedBackfillCheckpoint(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "without_gc"
		if cleanup {
			name = "with_gc"
		}
		t.Run(name, func(t *testing.T) { technicalHistoryPaused(t, cleanup) })
	}
}

func technicalHistoryPaused(t *testing.T, cleanup bool) {
	s, p, _ := setup(t)
	store := testStore(s)
	ctx := context.Background()
	accepted(t, s, p)
	runPages(t, s, 2)
	now := time.Now().UTC().Truncate(time.Second)
	op, err := s.Apply(ctx, serviceapi.Command{CommandID: "recover-backfill", Kind: "backfill", InitialDays: 2, RetentionDays: 30, From: now.Add(-20 * 24 * time.Hour).Unix(), To: now.Add(-19 * 24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Kind != "backfill" {
		t.Fatalf("expected backfill: %+v", claim)
	}
	if err := store.Fail(ctx, claim, serviceapi.Fail(serviceapi.ReauthRequired, "reauth")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_jobs SET updated_at=now()-interval '8 days' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_operations SET updated_at=now()-interval '8 days' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if cleanup {
		if _, err := store.Retain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Apply(ctx, serviceapi.Command{CommandID: "reauthorized", Kind: "sync", InitialDays: 2, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	var resumed bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_jobs WHERE id=$1 AND status='queued')`, claim.ID).Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("reauth-required backfill checkpoint was deleted after 8 days despite 30-day event retention; sync cannot resume it")
	}
}
