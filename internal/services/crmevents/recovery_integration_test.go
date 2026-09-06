package crmevents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestLongDowntimeRecoveryKeepsCheckpointAndUnfinishedPageAcrossReconstruction(t *testing.T) {
	s, policy, gateway := setup(t)
	pool := testStore(s).pool
	now := time.Now().UTC().Truncate(time.Second).Add(-21 * 24 * time.Hour)
	checkpoint := now
	cfg := DefaultConfig()
	cfg.Window = 24 * time.Hour
	cfg.Now = func() time.Time { return now }
	s = New(pool, policy, gateway, cfg)
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	runPages(t, s, 2) // Empty fully traversed/replayed day establishes continuous coverage.
	initial, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || initial.VerifiedThrough != checkpoint.Unix() {
		t.Fatalf("seed checkpoint=%+v err=%v", initial, err)
	}
	now = checkpoint.Add(21 * 24 * time.Hour)
	s = New(pool, policy, gateway, cfg)
	if _, err = pool.Exec(context.Background(), `UPDATE event_sources SET next_poll_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	var fixedFrom, fixedTo, fixedTarget time.Time
	var nextPage int
	if err = pool.QueryRow(context.Background(), `SELECT window_from,window_to,target_to,page FROM event_jobs WHERE status='queued' AND kind='current'`).Scan(&fixedFrom, &fixedTo, &fixedTarget, &nextPage); err != nil {
		t.Fatal(err)
	}
	if want := checkpoint.Add(-cfg.Overlap); !fixedFrom.Equal(want) || !fixedTarget.Equal(now) || nextPage != 1 {
		t.Fatalf("lost downtime range from=%s want=%s target=%s now=%s page=%d", fixedFrom, want, fixedTarget, now, nextPage)
	}
	if !fixedFrom.Before(now.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("recovery fell back to initial/lookback horizon: %s", fixedFrom)
	}
	var calls []serviceapi.EventPageRequest
	gateway.events = func(request serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		calls = append(calls, request)
		if request.Page == 1 {
			return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "before-reconstruction", CreatedAt: request.From + 1}}, HasNext: true}, nil
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "after-reconstruction", CreatedAt: request.From + 2}}}, nil
	}
	runPages(t, s, 1)
	if err = pool.QueryRow(context.Background(), `SELECT page FROM event_jobs WHERE status='queued' AND kind='current'`).Scan(&nextPage); err != nil || nextPage != 2 {
		t.Fatalf("page checkpoint=%d err=%v", nextPage, err)
	}
	// Reconstruct the business service from its database after another five days.
	// This tests restart state; it deliberately does not claim an OS-process crash.
	now = now.Add(5 * 24 * time.Hour)
	resumed := New(pool, policy, gateway, cfg)
	_, err = resumed.Apply(context.Background(), serviceapi.Command{CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	runPages(t, resumed, 1)
	if len(calls) != 2 || calls[1].Page != 2 || calls[1].From != fixedFrom.Unix() || calls[1].To != fixedTo.Unix() {
		t.Fatalf("unfinished fixed cursor reset across reconstruction: %+v", calls)
	}
	var target time.Time
	var jobs int
	if err = pool.QueryRow(context.Background(), `SELECT target_to FROM event_jobs WHERE status='queued' AND kind='current'`).Scan(&target); err != nil || !target.Equal(fixedTarget) {
		t.Fatalf("target moved after restart: %s want=%s err=%v", target, fixedTarget, err)
	}
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM event_jobs WHERE kind='current'`).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("manual resume/scheduler duplicated durable job: %d %v", jobs, err)
	}
	status, err := resumed.Status(context.Background(), serviceapi.Auth{})
	if err != nil || status.VerifiedThrough != checkpoint.Unix() {
		t.Fatalf("partial resumed pages incorrectly filled downtime: %+v %v", status, err)
	}
}

func TestRecentBackfillIslandCannotHideOlderDowntimeGap(t *testing.T) {
	s, policy, gateway := setup(t)
	pool := testStore(s).pool
	present := time.Now().UTC().Truncate(time.Second)
	now := present.Add(-10 * 24 * time.Hour)
	checkpoint := now
	cfg := DefaultConfig()
	cfg.Window = 24 * time.Hour
	cfg.Now = func() time.Time { return now }
	s = New(pool, policy, gateway, cfg)
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	runPages(t, s, 2)
	now = present
	s = New(pool, policy, gateway, cfg)
	from, to := present.Add(-24*time.Hour), present.Add(-time.Hour)
	op, err := s.Apply(context.Background(), serviceapi.Command{CommandID: uuid.NewString(), Kind: "backfill", From: from.Unix(), To: to.Unix(), InitialDays: 1, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	runPages(t, s, 2)
	if state := opState(t, s, op.ID).State; state != serviceapi.OperationSucceeded {
		t.Fatalf("backfill did not finish: %s", state)
	}
	status, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || status.VerifiedThrough != checkpoint.Unix() {
		t.Fatalf("recent island falsely closed ten-day gap: %+v %v", status, err)
	}
	var ranges int
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM event_coverage`).Scan(&ranges); err != nil || ranges != 2 {
		t.Fatalf("disconnected coverage ranges=%d err=%v", ranges, err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE event_sources SET next_poll_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recoveryFrom, recoveryTarget time.Time
	if err = pool.QueryRow(context.Background(), `SELECT window_from,target_to FROM event_jobs WHERE status='queued' AND kind='current'`).Scan(&recoveryFrom, &recoveryTarget); err != nil {
		t.Fatal(err)
	}
	if !recoveryFrom.Equal(checkpoint.Add(-cfg.Overlap)) || !recoveryTarget.Equal(present) {
		t.Fatalf("scheduler used recent backfill instead of continuous checkpoint: from=%s target=%s", recoveryFrom, recoveryTarget)
	}
}
