package crmevents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func insertOldTerminalJobs(t *testing.T, store *Postgres, installation uuid.UUID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := store.pool.Exec(context.Background(), `INSERT INTO event_jobs(id,installation_id,kind,priority,window_from,window_to,target_to,status,processed,inserted,updated_at) VALUES($1,$2,'backfill',100,now()-interval '2 hours',now()-interval '1 hour',now()-interval '1 hour','completed',1000,1000,now()-interval '8 days')`, uuid.New(), installation)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTechnicalHistoryReplayWithinHorizonAndAfterGC(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	ctx := context.Background()
	op := accepted(t, s, p)
	runPages(t, s, 2)
	replay, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil || replay.ID != op.ID || replay.Processed != opState(t, s, op.ID).Processed {
		t.Fatalf("in-horizon replay %+v want %s err=%v", replay, op.ID, err)
	}
	if _, err = store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	kept, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil || kept.ID != op.ID {
		t.Fatalf("Retain within horizon dropped identity %+v %v", kept, err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_jobs SET status='completed',updated_at=now()-interval '8 days' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_operations SET status='completed',updated_at=now()-interval '8 days' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_inbox SET created_at=now()-interval '8 days' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs, inbox, ops int
	if err = store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_jobs WHERE installation_id=$1),(SELECT count(*) FROM event_inbox WHERE installation_id=$1 AND command_id='start'),(SELECT count(*) FROM event_operations WHERE installation_id=$1)`, p.principal.InstallationID).Scan(&jobs, &inbox, &ops); err != nil || jobs != 0 || inbox != 0 || ops != 0 {
		t.Fatalf("receipt GC jobs=%d inbox=%d ops=%d err=%v", jobs, inbox, ops, err)
	}
	if _, err = s.Operation(ctx, serviceapi.OperationRequest{OperationID: op.ID}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("GCd operation still readable: %v", err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("late replay after receipt GC: %v", err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync", InitialDays: 3}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("late changed payload after receipt GC: %v", err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "fresh-after-gc", Kind: "sync", InitialDays: 2, RetentionDays: 14}); err != nil {
		t.Fatal(err)
	}
}

func TestTechnicalHistoryCleanupIsBoundedAndMayLag(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	store.cfg.RetentionBatch = 2
	store.cfg.HistoryBatch = 2
	accepted(t, s, p)
	ctx := context.Background()
	at := seedRetainedEvents(t, store, p.principal.InstallationID, 5)
	insertOldTerminalJobs(t, store, p.principal.InstallationID, 5)
	var frontierBefore *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT retained_from FROM event_sources WHERE installation_id=$1`, p.principal.InstallationID).Scan(&frontierBefore); err != nil {
		t.Fatal(err)
	}
	n, err := store.Retain(ctx)
	if err != nil || n != 2 {
		t.Fatalf("event batch %d %v", n, err)
	}
	var events, jobs int
	if err = store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM crm_events WHERE installation_id=$1),(SELECT count(*) FROM event_jobs WHERE installation_id=$1 AND status='completed')`, p.principal.InstallationID).Scan(&events, &jobs); err != nil {
		t.Fatal(err)
	}
	if events != 3 || jobs != 3 {
		t.Fatalf("cleanup looped unbounded events=%d jobs=%d", events, jobs)
	}
	var frontier time.Time
	if err = store.pool.QueryRow(ctx, `SELECT retained_from FROM event_sources WHERE installation_id=$1`, p.principal.InstallationID).Scan(&frontier); err != nil {
		t.Fatal(err)
	}
	if frontierBefore != nil && frontier.Before(*frontierBefore) {
		t.Fatalf("frontier rewound %v -> %v", frontierBefore, frontier)
	}
	if !frontier.Equal(at.Add(3 * time.Second)) {
		t.Fatalf("frontier %v want %v", frontier, at.Add(3*time.Second))
	}
}

func TestTechnicalHistoryKeepsLiveLeaseAndNonTerminalReceipt(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	op := accepted(t, s, p)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE event_jobs SET updated_at=now()-interval '8 days' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_operations SET updated_at=now()-interval '8 days' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_inbox SET created_at=now()-interval '8 days' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_sources SET lease_token=42,lease_until=now()+interval '1 hour' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_jobs SET status='paused',lease_token=42 WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs, inbox int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_jobs WHERE installation_id=$1),(SELECT count(*) FROM event_inbox WHERE operation_id=$2)`, p.principal.InstallationID, op.ID).Scan(&jobs, &inbox); err != nil || jobs != 1 || inbox != 1 {
		t.Fatalf("live lease/non-terminal GC jobs=%d inbox=%d err=%v", jobs, inbox, err)
	}
	replay, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil || replay.ID != op.ID {
		t.Fatalf("non-terminal replay %+v %v", replay, err)
	}
}

func TestTechnicalHistoryDoesNotFollowShorterRetentionDays(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	op := accepted(t, s, p)
	ctx := context.Background()
	runPages(t, s, 2)
	if _, err := store.pool.Exec(ctx, `UPDATE event_sources SET retention_days=2 WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_jobs SET status='completed',updated_at=now()-interval '3 days' WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE event_operations SET status='completed',updated_at=now()-interval '3 days' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	var inbox int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM event_inbox WHERE command_id='start'`).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("retention_days shortened history inbox=%d err=%v", inbox, err)
	}
	replay, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil || replay.ID != op.ID {
		t.Fatalf("3-day receipt lost %+v %v", replay, err)
	}
}

func TestTechnicalHistoryMetricsStayFiniteAfterCompletedJobs(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	accepted(t, s, p)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE event_sources SET events_processed=11,events_inserted=4 WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	before, err := store.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Processed != 11 || before.Inserted != 4 {
		t.Fatalf("source counters %+v", before)
	}
	if _, ok := before.States["completed"]; ok {
		t.Fatalf("completed gauge from history: %+v", before.States)
	}
	if before.States["queued"] != 1 {
		t.Fatalf("active gauge %+v", before.States)
	}
	insertOldTerminalJobs(t, store, p.principal.InstallationID, 20)
	mid, err := store.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mid.States["completed"]; ok {
		t.Fatalf("completed jobs inflated gauge: %+v", mid.States)
	}
	if mid.Processed != 11 || mid.Inserted != 4 {
		t.Fatalf("job history changed cumulative counters %+v", mid)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_jobs SET status=$1 WHERE status='queued'`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.States["other"] == 0 {
		t.Fatalf("unknown status dropped from gauges: %+v", unknown.States)
	}
	store.cfg.HistoryBatch = 100
	if _, err = store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Processed != 11 || after.Inserted != 4 {
		t.Fatalf("job GC reset counters %+v", after)
	}
}

func TestTechnicalHistoryCoverageAndEnrichmentBounds(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	accepted(t, s, p)
	ctx := context.Background()
	id := p.principal.InstallationID
	retained := time.Now().UTC().Truncate(time.Second).Add(-7 * 24 * time.Hour)
	if _, err := store.pool.Exec(ctx, `UPDATE event_sources SET retained_from=$2 WHERE installation_id=$1`, id, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO event_coverage(installation_id,window_from,window_to) VALUES
		($1,$2,$3),($1,$3,$4),($1,$5,$6)`, id, retained.Add(-3*time.Hour), retained.Add(-time.Hour), retained, retained.Add(-time.Minute), retained.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,content_hash)
		VALUES($1,'keep',now(),7,'test',1,'lead','hash'::bytea)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,state,reason_code,source)
		VALUES($1,'entity','linked','ready','',''),($1,'pipeline','orphan','ready','','')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO event_enrichment_links(installation_id,event_id,object_kind,object_key) VALUES($1,'keep','entity','linked')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	var behind, overlap, linked, orphan int
	if err := store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM event_coverage WHERE installation_id=$1 AND window_to<=$2),
		(SELECT count(*) FROM event_coverage WHERE installation_id=$1 AND window_to>$2),
		(SELECT count(*) FROM event_enrichment_objects WHERE installation_id=$1 AND object_key='linked'),
		(SELECT count(*) FROM event_enrichment_objects WHERE installation_id=$1 AND object_key='orphan')`, id, retained).Scan(&behind, &overlap, &linked, &orphan); err != nil {
		t.Fatal(err)
	}
	if behind != 0 || overlap != 1 || linked != 1 || orphan != 0 {
		t.Fatalf("coverage/enrichment behind=%d overlap=%d linked=%d orphan=%d", behind, overlap, linked, orphan)
	}
}

func TestTechnicalHistoryNormalizeRejectsShorterHorizon(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HistoryHorizon = time.Hour
	got := normalizeConfig(cfg)
	if got.HistoryHorizon != TechnicalHistoryHorizon {
		t.Fatalf("horizon shortened to %s", got.HistoryHorizon)
	}
}
