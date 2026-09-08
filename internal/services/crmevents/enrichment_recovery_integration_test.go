package crmevents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func seedRecoveryObject(t *testing.T, s *Service, p *testPolicy, kind, key string, id int64) {
	t.Helper()
	_, err := testStore(s).pool.Exec(context.Background(), `INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,parent_type,parent_id,object_id) VALUES($1,$2,$3,'leads',1,$4)`, p.principal.InstallationID, kind, key, id)
	if err != nil {
		t.Fatal(err)
	}
}
func TestEnrichmentRecoversAfterFastRetriesExhausted(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	seedRecoveryObject(t, s, p, "note", "1", 1)
	g := newFixtureGateway(t)
	g.notesErr = serviceapi.Fail(serviceapi.Unavailable, "outage")
	g.notes = []serviceapi.Note{{ID: 1, EntityType: "leads", EntityID: 1, Params: []byte(`{"text":"restored"}`)}}
	s.gateway = g
	ctx := context.Background()
	store := testStore(s)
	for i := 0; i < 5; i++ {
		if _, err := store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET run_after=now()-interval '1 second'`); err != nil {
			t.Fatal(err)
		}
		if worked, err := s.EnrichOnce(ctx); err != nil || !worked {
			t.Fatalf("attempt %d %v %v", i, worked, err)
		}
	}
	var state string
	var attempts int
	var delay float64
	if err := store.pool.QueryRow(ctx, `SELECT state,attempts,extract(epoch from run_after-now()) FROM event_enrichment_objects`).Scan(&state, &attempts, &delay); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 5 || delay < 590 || delay > 600 {
		t.Fatalf("state=%s attempts=%d delay=%v", state, attempts, delay)
	}
	if worked, err := s.EnrichOnce(ctx); err != nil || worked {
		t.Fatalf("slow retry ignored: %v %v", worked, err)
	}
	g.notesErr = nil
	if _, err := store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET run_after=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.EnrichOnce(ctx); err != nil || !worked {
		t.Fatalf("recover: %v %v", worked, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state,attempts FROM event_enrichment_objects`).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || attempts != 0 {
		t.Fatalf("recovery state=%s attempts=%d", state, attempts)
	}
}
func TestEnrichmentClaimAdmissionIsAtomic(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	seedRecoveryObject(t, s, p, "entity", "leads:1", 1)
	seedRecoveryObject(t, s, p, "note", "1", 1)
	ctx := context.Background()
	store := testStore(s)
	_, err := store.pool.Exec(ctx, `CREATE FUNCTION recovery_claim_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.5); RETURN NEW; END $$; CREATE TRIGGER recovery_claim_delay BEFORE UPDATE ON event_enrichment_objects FOR EACH ROW EXECUTE FUNCTION recovery_claim_delay()`)
	if err != nil {
		t.Fatal(err)
	}
	defer store.pool.Exec(ctx, `DROP TRIGGER recovery_claim_delay ON event_enrichment_objects; DROP FUNCTION recovery_claim_delay()`)
	claims := make([]EnrichmentClaim, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range claims {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; claims[i], errs[i] = store.ClaimEnrichment(ctx) }(i)
	}
	close(start)
	wg.Wait()
	success := 0
	var old EnrichmentClaim
	for i, err := range errs {
		if err == nil {
			success++
			old = claims[i]
		} else if !errors.Is(err, ErrNoWork) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("expected one active claim, got %d (%v)", success, errs)
	}
	// A dead worker's expired lease can be reclaimed; stale finalization is fenced.
	if _, err = store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET lease_until=now()-interval '1 second',run_after=CASE WHEN object_kind=$1 THEN now()-interval '1 hour' ELSE now()+interval '1 hour' END`, old.Kind); err != nil {
		t.Fatal(err)
	}
	newer, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Objects[0].Token <= old.Objects[0].Token {
		t.Fatal("lease fencing token did not advance")
	}
	if err = store.SaveEnrichment(ctx, old, []enrichmentSave{{Key: old.Objects[0].Key, State: "ready", Payload: []byte(`{"stale":true}`)}}); err != nil {
		t.Fatal(err)
	}
	var stale bool
	if err = store.pool.QueryRow(ctx, `SELECT payload ? 'stale' FROM event_enrichment_objects WHERE object_kind=$1`, old.Kind).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("expired worker overwrote the new claim")
	}
}

type partialBatchGateway struct{ testGateway }

func (*partialBatchGateway) Notes(context.Context, serviceapi.NotesRequest) (serviceapi.NotePage, error) {
	page := serviceapi.NotePage{InvalidIDs: []int64{50}}
	for i := int64(1); i < 50; i++ {
		page.Notes = append(page.Notes, serviceapi.Note{ID: i, EntityType: "leads", EntityID: 1, Params: []byte(`{"text":"valid"}`)})
	}
	return page, nil
}
func TestEnrichmentPartialBatchSavesHealthyObjects(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	s.gateway = &partialBatchGateway{}
	for i := int64(1); i <= 50; i++ {
		seedRecoveryObject(t, s, p, "note", fmt.Sprint(i), i)
	}
	if worked, err := s.EnrichOnce(context.Background()); err != nil || !worked {
		t.Fatalf("enrich %v %v", worked, err)
	}
	var ready, invalid int
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE state='ready'),count(*) FILTER(WHERE state='unavailable' AND reason_code='invalid' AND payload='null'::jsonb) FROM event_enrichment_objects`).Scan(&ready, &invalid); err != nil {
		t.Fatal(err)
	}
	if ready != 49 || invalid != 1 {
		t.Fatalf("ready=%d invalid=%d", ready, invalid)
	}
	if worked, err := s.EnrichOnce(context.Background()); err != nil || worked {
		t.Fatalf("unexpected immediate retry: %v %v", worked, err)
	}
}
func TestEnrichmentMigrationResumesLegacyTemporaryErrors(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	seedRecoveryObject(t, s, p, "note", "1", 1)
	seedRecoveryObject(t, s, p, "note", "2", 2)
	ctx := context.Background()
	store := testStore(s)
	_, err := store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET state='error',reason_code=CASE WHEN object_id=1 THEN 'temporary' ELSE 'invalid' END,attempts=5`)
	if err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../../../migrations/crmevents/000006_enrichment_retry_recovery.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	c, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Objects) != 1 || c.Objects[0].ObjectID != 1 || c.Objects[0].Attempts != 0 {
		t.Fatalf("claim %+v", c)
	}
}

func TestEnrichmentReauthWaitIsBoundedAndSlow(t *testing.T) {
	for _, attempt := range []int{1, 5, 100} {
		state, reason, delay := enrichmentFailure(serviceapi.Fail(serviceapi.ReauthRequired, "reauth"), attempt, 5)
		if state != "retry" || reason != "temporary" || delay != time.Hour {
			t.Fatalf("reauth %s %s %v", state, reason, delay)
		}
	}
}
