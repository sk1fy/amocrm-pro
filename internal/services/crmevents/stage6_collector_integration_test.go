package crmevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// REL-01: collector guarantees after the enrichment model expansion.
// These cases reuse store helpers (setup/accepted/runPages/testStore) and do
// not introduce a second collector. Synthetic payloads only.

func rel01NoteEvent(id string, at, noteID, entityID int64) serviceapi.Event {
	return serviceapi.Event{
		ID: id, CreatedAt: at, CreatedBy: 7, Type: "common_note_added",
		EntityID: entityID, EntityType: "lead",
		ValueAfter: json.RawMessage(`[{"note":{"id":` + strconv.FormatInt(noteID, 10) + `}}]`),
	}
}

func rel01JSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	decode := func(raw []byte) any {
		t.Helper()
		if len(raw) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	ga, err := json.Marshal(decode(a))
	if err != nil {
		t.Fatal(err)
	}
	gb, err := json.Marshal(decode(b))
	if err != nil {
		t.Fatal(err)
	}
	return string(ga) == string(gb)
}

func rel01Hash(t *testing.T, s *Service, installation uuid.UUID, eventID string) []byte {
	t.Helper()
	var hash []byte
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT content_hash FROM crm_events WHERE installation_id=$1 AND event_id=$2`, installation, eventID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), hash...)
}

func rel01SourceLease(t *testing.T, s *Service, installation uuid.UUID) (token int64, until *time.Time, state, errorCode string) {
	t.Helper()
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT lease_token,lease_until,state,error_code FROM event_sources WHERE installation_id=$1`, installation).Scan(&token, &until, &state, &errorCode); err != nil {
		t.Fatal(err)
	}
	return token, until, state, errorCode
}

func rel01Coverage(t *testing.T, s *Service, installation uuid.UUID) (n int, from, to *time.Time) {
	t.Helper()
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT count(*),min(window_from),max(window_to) FROM event_coverage WHERE installation_id=$1`, installation).Scan(&n, &from, &to); err != nil {
		t.Fatal(err)
	}
	return n, from, to
}

func rel01Continuous(t *testing.T, s *Service, installation uuid.UUID) (from, to *time.Time) {
	t.Helper()
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT continuous_from,continuous_to FROM event_sources WHERE installation_id=$1`, installation).Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	return from, to
}

func rel01ObjectLease(t *testing.T, s *Service, installation uuid.UUID, kind, key string) (token int64, until *time.Time, state, reason string, payload []byte) {
	t.Helper()
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT lease_token,lease_until,state,reason_code,payload FROM event_enrichment_objects WHERE installation_id=$1 AND object_kind=$2 AND object_key=$3`, installation, kind, key).Scan(&token, &until, &state, &reason, &payload); err != nil {
		t.Fatal(err)
	}
	return token, until, state, reason, append([]byte(nil), payload...)
}

func rel01ResetEnrichmentDue(t *testing.T, s *Service, installation uuid.UUID) {
	t.Helper()
	if _, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_enrichment_objects SET run_after=now()-interval '1 second' WHERE installation_id=$1`, installation); err != nil {
		t.Fatal(err)
	}
}

func rel01ResetJobsDue(t *testing.T, s *Service, installation uuid.UUID) {
	t.Helper()
	if _, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET run_after=now() WHERE installation_id=$1`, installation); err != nil {
		t.Fatal(err)
	}
}

func rel01TimesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// Restart, crash after write, fencing, page replay, late event and unstable
// pagination with enrichment objects queued/saved. Sidecar must not change
// content_hash; enrichment must not take the collector source lease; a fenced
// stale enrichment writer must not overwrite a newer object.
func TestCollectorGuaranteesHoldWithQueuedEnrichment(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	ctx := context.Background()
	installation := p.principal.InstallationID
	at := time.Now().Add(-time.Minute).Unix()
	note := rel01NoteEvent("note-a", at, 77, 31001)
	other := serviceapi.Event{ID: "lead-b", CreatedAt: at, CreatedBy: 7, Type: "lead_added", EntityID: 31001, EntityType: "lead"}
	late := rel01NoteEvent("note-late", at, 78, 31001)
	tail := serviceapi.Event{ID: "tail", CreatedAt: at, CreatedBy: 8, Type: "lead_added", EntityID: 31002, EntityType: "lead"}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		if r.Page == 1 {
			return serviceapi.EventPage{Events: []serviceapi.Event{note, other, late}, HasNext: true}, nil
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{tail}}, nil
	}

	c, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sourceToken, until, _, _ := rel01SourceLease(t, s, installation)
	if sourceToken != c.Token || until == nil || !until.After(time.Now()) {
		t.Fatalf("collector lease not live: token=%d until=%v claim=%d", sourceToken, until, c.Token)
	}
	if _, err = store.ClaimEnrichment(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("enrichment admitted under live collector lease: %v", err)
	}
	tokenAfter, _, _, _ := rel01SourceLease(t, s, installation)
	if tokenAfter != sourceToken {
		t.Fatalf("enrichment took collector lease_token %d -> %d", sourceToken, tokenAfter)
	}

	if err = store.SavePage(ctx, c, serviceapi.EventPage{Events: []serviceapi.Event{note, other}, HasNext: true}); err != nil {
		t.Fatal(err)
	}
	hashA := rel01Hash(t, s, installation, "note-a")
	hashB := rel01Hash(t, s, installation, "lead-b")
	var queued int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM event_enrichment_objects WHERE installation_id=$1 AND state='pending'`, installation).Scan(&queued); err != nil || queued == 0 {
		t.Fatalf("expected queued enrichment after page write, n=%d err=%v", queued, err)
	}

	// Crash after write: reconstruct the service from the owner DB. The page
	// cursor and queued objects must survive; the committed events stay.
	s = New(store.pool, p, g, s.cfg)
	store = testStore(s)
	listed, err := s.Query(ctx, serviceapi.Query{From: at - 1, To: at + 1, Limit: 10})
	if err != nil || len(listed.Events) != 2 {
		t.Fatalf("crash lost page write %+v %v", listed, err)
	}
	if !rel01JSONEqual(t, note.ValueAfter, listed.Events[0].ValueAfter) && !rel01JSONEqual(t, note.ValueAfter, listed.Events[1].ValueAfter) {
		t.Fatalf("stored note JSON meaning changed: %+v", listed.Events)
	}

	stale, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_sources SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Claim(ctx)
	if err != nil || fresh.Token <= stale.Token {
		t.Fatalf("replacement %+v %v", fresh, err)
	}
	if err = store.SavePage(ctx, stale, serviceapi.EventPage{Events: []serviceapi.Event{late}, HasNext: true}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale collector writer accepted: %v", err)
	}
	if !bytes.Equal(rel01Hash(t, s, installation, "note-a"), hashA) {
		t.Fatal("stale collector writer mutated content_hash")
	}
	if err = store.SavePage(ctx, fresh, serviceapi.EventPage{Events: []serviceapi.Event{tail}}); err != nil {
		t.Fatal(err)
	}
	// Pass 2 introduces the late ID; pass 3 matches and covers the window.
	runPages(t, s, 4)
	var count int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM crm_events WHERE installation_id=$1`, installation).Scan(&count); err != nil || count != 4 {
		t.Fatalf("late event missing after replay count=%d err=%v", count, err)
	}
	if !bytes.Equal(rel01Hash(t, s, installation, "note-a"), hashA) || !bytes.Equal(rel01Hash(t, s, installation, "lead-b"), hashB) {
		t.Fatal("page replay changed historical content_hash")
	}
	before, err := s.Status(ctx, serviceapi.Auth{})
	if err != nil || before.VerifiedThrough == 0 {
		t.Fatalf("stabilized window missing coverage %+v %v", before, err)
	}

	// Enrichment fencing against a newer object lease. Collector source lease
	// must stay untouched. Done before the unstable window, which fails the source.
	rel01ResetEnrichmentDue(t, s, installation)
	sourceToken, _, sourceState, _ := rel01SourceLease(t, s, installation)
	old, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tokenDuring, untilDuring, _, _ := rel01SourceLease(t, s, installation)
	if tokenDuring != sourceToken || untilDuring != nil {
		t.Fatalf("enrichment occupied event_sources lease token=%d until=%v", tokenDuring, untilDuring)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET lease_until=now()-interval '1 second' WHERE installation_id=$1`, installation); err != nil {
		t.Fatal(err)
	}
	newer, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Objects[0].Token <= old.Objects[0].Token {
		t.Fatal("enrichment fencing token did not advance")
	}
	if err = store.SaveEnrichment(ctx, newer, []enrichmentSave{{Key: newer.Objects[0].Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceNotesAPI, Payload: json.RawMessage(`{"text":"fresh"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveEnrichment(ctx, old, []enrichmentSave{{Key: old.Objects[0].Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceNotesAPI, Payload: json.RawMessage(`{"text":"stale"}`)}}); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, payload := rel01ObjectLease(t, s, installation, newer.Kind, newer.Objects[0].Key)
	if !rel01JSONEqual(t, payload, []byte(`{"text":"fresh"}`)) {
		t.Fatalf("stale enrichment writer overwrote newer object: %s", payload)
	}
	if !bytes.Equal(rel01Hash(t, s, installation, "note-a"), hashA) {
		t.Fatal("enrichment sidecar changed stored content_hash")
	}
	detail, err := s.GetEvent(ctx, serviceapi.EventRequest{EventID: "note-a"})
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, err := canonicalEvent(detail)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	if string(sum[:]) != string(hashA) {
		t.Fatal("GetEvent sidecar changed canonical event hash")
	}
	if sourceState == "failed" || sourceState == "reauth_required" {
		t.Fatalf("collector source failed from enrichment path: %s", sourceState)
	}

	_, err = s.Apply(ctx, serviceapi.Command{CommandID: "unstable-enrich", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{rel01NoteEvent(uuid.NewString(), r.From+1, 77, 31001)}}, nil
	}
	runPages(t, s, 3)
	after, err := s.Status(ctx, serviceapi.Auth{})
	if err != nil || after.ErrorCode != "pagination_unstable" || after.VerifiedThrough != before.VerifiedThrough {
		t.Fatalf("unstable pagination advanced coverage: %+v", after)
	}
	if !bytes.Equal(rel01Hash(t, s, installation, "note-a"), hashA) {
		t.Fatal("unstable window changed historical content_hash")
	}
}

// Original collection Fail() semantics stay for 429/5xx/timeout/permission/reauth.
// FailEnrichment must not mark event_sources failed/reauth and must not move coverage.
func TestCollectorAndEnrichmentFailureIsolation(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	ctx := context.Background()
	installation := p.principal.InstallationID
	at := time.Now().Add(-time.Minute).Unix()
	note := rel01NoteEvent("iso-note", at, 91, 31001)
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{note}}, nil
	}
	runPages(t, s, 2)
	covN, covFrom, covTo := rel01Coverage(t, s, installation)
	contFrom, contTo := rel01Continuous(t, s, installation)
	sourceToken, _, sourceState, sourceErr := rel01SourceLease(t, s, installation)
	if sourceState == "failed" || contTo == nil {
		t.Fatalf("collection did not establish idle coverage state=%s through=%v", sourceState, contTo)
	}

	enrichmentCauses := []struct {
		name        string
		cause       error
		wantState   string
		wantReason  string
		sourceMust  string
		sourceError string
	}{
		{"429", serviceapi.Fail(serviceapi.ResourceExhausted, "rate"), serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, sourceState, sourceErr},
		{"5xx", serviceapi.Fail(serviceapi.Unavailable, "upstream"), serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, sourceState, sourceErr},
		{"timeout", serviceapi.Fail(serviceapi.DeadlineExceeded, "deadline"), serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, sourceState, sourceErr},
		{"permission", serviceapi.Fail(serviceapi.PermissionDenied, "scope"), serviceapi.EnrichmentUnavailable, serviceapi.ReasonPermissionDenied, sourceState, sourceErr},
		{"reauth", serviceapi.Fail(serviceapi.ReauthRequired, "expired"), serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, sourceState, sourceErr},
	}
	for _, tc := range enrichmentCauses {
		rel01ResetEnrichmentDue(t, s, installation)
		claim, err := store.ClaimEnrichment(ctx)
		if err != nil {
			t.Fatalf("%s claim: %v", tc.name, err)
		}
		if err = store.FailEnrichment(ctx, claim, tc.cause); err != nil {
			t.Fatalf("%s FailEnrichment: %v", tc.name, err)
		}
		_, _, objState, objReason, _ := rel01ObjectLease(t, s, installation, claim.Kind, claim.Objects[0].Key)
		if objState != tc.wantState || objReason != tc.wantReason {
			t.Fatalf("%s object state=%s reason=%s", tc.name, objState, objReason)
		}
		token, until, state, code := rel01SourceLease(t, s, installation)
		if token != sourceToken || until != nil || state != tc.sourceMust || code != tc.sourceError {
			t.Fatalf("%s mutated source token=%d until=%v state=%s code=%s", tc.name, token, until, state, code)
		}
		if state == "failed" || state == "reauth_required" {
			t.Fatalf("%s FailEnrichment marked source %s", tc.name, state)
		}
		n, from, to := rel01Coverage(t, s, installation)
		cf, ct := rel01Continuous(t, s, installation)
		if n != covN || !rel01TimesEqual(from, covFrom) || !rel01TimesEqual(to, covTo) || !rel01TimesEqual(cf, contFrom) || !rel01TimesEqual(ct, contTo) {
			t.Fatalf("%s moved coverage n=%d from=%v to=%v continuous=%v..%v", tc.name, n, from, to, cf, ct)
		}
	}

	_, err := s.Apply(ctx, serviceapi.Command{CommandID: "iso-collect-fail", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	collectorCauses := []struct {
		name      string
		cause     error
		wantState string
	}{
		{"429", serviceapi.Fail(serviceapi.ResourceExhausted, "rate"), "retry"},
		{"5xx", serviceapi.Fail(serviceapi.Unavailable, "upstream"), "retry"},
		{"timeout", serviceapi.Fail(serviceapi.DeadlineExceeded, "deadline"), "retry"},
		{"permission", serviceapi.Fail(serviceapi.PermissionDenied, "scope"), "paused"},
		{"reauth", serviceapi.Fail(serviceapi.ReauthRequired, "expired"), "reauth_required"},
	}
	resume := false
	for _, tc := range collectorCauses {
		if resume {
			if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "iso-resume-" + tc.name, Kind: "sync"}); err != nil {
				t.Fatal(err)
			}
			resume = false
		}
		rel01ResetJobsDue(t, s, installation)
		g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
			return serviceapi.EventPage{}, tc.cause
		}
		worked, err := s.RunOnce(ctx)
		if err != nil || !worked {
			t.Fatalf("%s collect RunOnce worked=%t err=%v", tc.name, worked, err)
		}
		_, _, state, code := rel01SourceLease(t, s, installation)
		if state != tc.wantState || code != string(serviceapi.ErrorCode(tc.cause)) {
			t.Fatalf("%s collector source state=%s code=%s", tc.name, state, code)
		}
		n, from, to := rel01Coverage(t, s, installation)
		_, ct := rel01Continuous(t, s, installation)
		if n != covN || !rel01TimesEqual(from, covFrom) || !rel01TimesEqual(to, covTo) || !rel01TimesEqual(ct, contTo) {
			t.Fatalf("%s collector Fail moved coverage n=%d to=%v", tc.name, n, ct)
		}
		status, err := s.Status(ctx, serviceapi.Auth{})
		if err != nil || status.VerifiedThrough != contTo.Unix() {
			t.Fatalf("%s VerifiedThrough moved %+v", tc.name, status)
		}
		if tc.wantState == "paused" {
			resume = true
		}
		if tc.wantState == "reauth_required" {
			if worked, err = s.RunOnce(ctx); worked || err != nil {
				t.Fatalf("reauth source kept collecting %t %v", worked, err)
			}
			if status.ReauthRequired != true {
				t.Fatalf("collector reauth flag missing %+v", status)
			}
		}
	}
}

// Events of a window are saved first. Enrichment retry/unavailable must not
// block that window and must not advance coverage by itself.
func TestEnrichmentFailureDoesNotBlockOrAdvanceCoverage(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	ctx := context.Background()
	installation := p.principal.InstallationID
	at := time.Now().Add(-time.Minute).Unix()
	task := serviceapi.Event{ID: "win-task", CreatedAt: at, CreatedBy: 7, Type: "task_added", EntityID: 41001, EntityType: "lead"}
	tail := serviceapi.Event{ID: "win-tail", CreatedAt: at, CreatedBy: 8, Type: "lead_added", EntityID: 31002, EntityType: "lead"}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		if r.Page == 1 {
			return serviceapi.EventPage{Events: []serviceapi.Event{task}, HasNext: true}, nil
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{tail}}, nil
	}
	runPages(t, s, 1)
	got, err := s.Query(ctx, serviceapi.Query{From: at - 1, To: at + 1, Limit: 10})
	if err != nil || len(got.Events) != 1 || got.Events[0].ID != "win-task" {
		t.Fatalf("page not saved before enrichment %+v %v", got, err)
	}
	status, err := s.Status(ctx, serviceapi.Auth{})
	if err != nil || status.VerifiedThrough != 0 {
		t.Fatalf("partial page claimed coverage %+v %v", status, err)
	}
	hash := rel01Hash(t, s, installation, "win-task")

	fg := newFixtureGateway(t)
	fg.events = g.events
	fg.tasksErr = serviceapi.Fail(serviceapi.Unavailable, "tasks down")
	s.gateway = fg
	rel01ResetEnrichmentDue(t, s, installation)
	if worked, err := s.EnrichOnce(ctx); err != nil || !worked {
		t.Fatalf("enrich fail worked=%t err=%v", worked, err)
	}
	status, err = s.Status(ctx, serviceapi.Auth{})
	if err != nil || status.VerifiedThrough != 0 || status.State == "failed" {
		t.Fatalf("enrichment error advanced or failed window %+v %v", status, err)
	}
	n, _, _ := rel01Coverage(t, s, installation)
	if n != 0 {
		t.Fatalf("enrichment wrote coverage rows=%d", n)
	}

	runPages(t, s, 3)
	status, err = s.Status(ctx, serviceapi.Auth{})
	if err != nil || status.VerifiedThrough == 0 {
		t.Fatalf("collector window blocked by enrichment retry %+v %v", status, err)
	}
	through := status.VerifiedThrough
	var retrying int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM event_enrichment_objects WHERE installation_id=$1 AND state='retry'`, installation).Scan(&retrying); err != nil || retrying == 0 {
		t.Fatalf("expected enrichment retry alongside verified window n=%d err=%v", retrying, err)
	}
	listed, err := s.Query(ctx, serviceapi.Query{From: at - 1, To: at + 1, Limit: 10})
	if err != nil || len(listed.Events) != 2 {
		t.Fatalf("window events missing %+v %v", listed, err)
	}

	fg.tasksErr = nil
	rel01ResetEnrichmentDue(t, s, installation)
	drainEnrichment(t, s)
	status, err = s.Status(ctx, serviceapi.Auth{})
	if err != nil || status.VerifiedThrough != through {
		t.Fatalf("enrichment success moved coverage %+v want %d", status, through)
	}
	if !bytes.Equal(rel01Hash(t, s, installation, "win-task"), hash) {
		t.Fatal("later enrichment changed event content_hash")
	}
}

// Claim admits current collection on A before backfill on B. ClaimEnrichment
// refuses an installation whose collector job holds a live source lease and
// never takes event_sources.lease_token. Worker calls EnrichOnce only when
// Claim returned no work; this test checks that admission at the repository.
func TestCurrentCollectionPriorityOverBackfillAndEnrichment(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	ctx := context.Background()

	a := p.principal
	b := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 77, Consumer: "activity"}
	// Finish B's current job before A is queued. runPages claims globally, so
	// doing it after accepted(A) would consume A's current window instead.
	p.principal = b
	accepted(t, s, p)
	runPages(t, s, 2)
	now := time.Now().Unix()
	if _, err := s.Apply(ctx, serviceapi.Command{CommandID: "b-backfill", Kind: "backfill", From: now - 7200, To: now - 3600}); err != nil {
		t.Fatal(err)
	}
	seedRecoveryObject(t, s, p, "note", "2", 2)

	p.principal = a
	accepted(t, s, p)
	seedRecoveryObject(t, s, p, "note", "1", 1)

	var aTokenBefore, bTokenBefore int64
	if err := store.pool.QueryRow(ctx, `SELECT lease_token FROM event_sources WHERE installation_id=$1`, a.InstallationID).Scan(&aTokenBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT lease_token FROM event_sources WHERE installation_id=$1`, b.InstallationID).Scan(&bTokenBefore); err != nil {
		t.Fatal(err)
	}

	first, err := store.Claim(ctx)
	if err != nil || first.InstallationID != a.InstallationID || first.Kind != "current" {
		t.Fatalf("current A not first %+v %v", first, err)
	}

	enr, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enr.InstallationID != b.InstallationID {
		t.Fatalf("enrichment claimed busy collector tenant %+v", enr)
	}
	var aObjLease *time.Time
	if err = store.pool.QueryRow(ctx, `SELECT lease_until FROM event_enrichment_objects WHERE installation_id=$1 AND object_key='1'`, a.InstallationID).Scan(&aObjLease); err != nil {
		t.Fatal(err)
	}
	if aObjLease != nil {
		t.Fatal("enrichment leased objects on the collecting installation")
	}
	aToken, aUntil, _, _ := rel01SourceLease(t, s, a.InstallationID)
	bToken, bUntil, _, _ := rel01SourceLease(t, s, b.InstallationID)
	if aToken != first.Token || aUntil == nil || !aUntil.After(time.Now()) {
		t.Fatalf("collector A lease lost during enrichment claim token=%d until=%v", aToken, aUntil)
	}
	if bToken != bTokenBefore || bUntil != nil {
		t.Fatalf("enrichment took B source lease token %d->%d until=%v", bTokenBefore, bToken, bUntil)
	}
	var bObjectToken int64
	if err = store.pool.QueryRow(ctx, `SELECT lease_token FROM event_enrichment_objects WHERE installation_id=$1 AND object_key='2'`, b.InstallationID).Scan(&bObjectToken); err != nil || bObjectToken == 0 {
		t.Fatalf("B enrichment object lease missing token=%d err=%v", bObjectToken, err)
	}
	if err = store.FailEnrichment(ctx, enr, serviceapi.Fail(serviceapi.Unavailable, "release")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE event_enrichment_objects SET run_after=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}

	second, err := store.Claim(ctx)
	if err != nil || second.InstallationID != b.InstallationID || second.Kind != "backfill" {
		t.Fatalf("remaining worker capacity should take B backfill %+v %v", second, err)
	}
	if _, err = store.ClaimEnrichment(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("enrichment admitted while A current and B backfill hold live source leases: %v", err)
	}

	if _, err = store.pool.Exec(ctx, `UPDATE event_jobs SET status='completed'; UPDATE event_sources SET lease_until=NULL,state='idle'; UPDATE event_enrichment_objects SET lease_until=NULL,run_after=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("collector still had work after jobs completed: %v", err)
	}
	idle, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idle.InstallationID != a.InstallationID && idle.InstallationID != b.InstallationID {
		t.Fatalf("unexpected enrichment tenant %+v", idle)
	}
	if first.Token <= aTokenBefore {
		t.Fatalf("collector claim did not advance A source lease %d -> %d", aTokenBefore, first.Token)
	}
}

// Commands are identified by (installation, command_id) plus payload hash after
// normalizeCommand defaults. A later sync attaches to the existing current job
// and updates event_sources settings in place; it does not rewrite the durable
// window/cursor and does not restore retained events.
func TestSettingsSnapshotOnQueuedAndRunningJobs(t *testing.T) {
	s, p, _ := setup(t)
	store := testStore(s)
	ctx := context.Background()
	installation := p.principal.InstallationID

	first, err := s.Apply(ctx, serviceapi.Command{CommandID: "cfg-1", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	var windowFrom, windowTo, target time.Time
	var page, initialHours, retentionDays int
	var jobID uuid.UUID
	if err = store.pool.QueryRow(ctx, `SELECT j.id,j.window_from,j.window_to,j.target_to,j.page,s.initial_hours,s.retention_days FROM event_jobs j JOIN event_sources s USING(installation_id) WHERE j.installation_id=$1 AND j.kind='current'`, installation).Scan(&jobID, &windowFrom, &windowTo, &target, &page, &initialHours, &retentionDays); err != nil {
		t.Fatal(err)
	}
	if initialHours != 48 || retentionDays != 7 || page != 1 {
		t.Fatalf("default snapshot hours=%d retention=%d page=%d", initialHours, retentionDays, page)
	}

	replay, err := s.Apply(ctx, serviceapi.Command{CommandID: "cfg-1", Kind: "sync", InitialDays: 2, RetentionDays: 7})
	if err != nil || replay.ID != first.ID {
		t.Fatalf("normalized default replay %+v %v", replay, err)
	}
	_, err = s.Apply(ctx, serviceapi.Command{CommandID: "cfg-1", Kind: "sync", InitialDays: 3, RetentionDays: 7})
	if serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed payload reused command id: %v", err)
	}

	second, err := s.Apply(ctx, serviceapi.Command{CommandID: "cfg-2", Kind: "sync", InitialDays: 1, RetentionDays: 7})
	if err != nil || second.ID == first.ID {
		t.Fatalf("new command should attach as its own operation %+v first=%+v err=%v", second, first, err)
	}
	var attached int
	var from2, to2, target2 time.Time
	var hours2, retention2 int
	var jobs int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM event_operation_jobs WHERE job_id=$1`, jobID).Scan(&attached); err != nil || attached != 2 {
		t.Fatalf("expected both operations on the original job attached=%d err=%v", attached, err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT j.window_from,j.window_to,j.target_to,s.initial_hours,s.retention_days,(SELECT count(*) FROM event_jobs WHERE installation_id=$1 AND kind='current') FROM event_jobs j JOIN event_sources s USING(installation_id) WHERE j.id=$2`, installation, jobID).Scan(&from2, &to2, &target2, &hours2, &retention2, &jobs); err != nil {
		t.Fatal(err)
	}
	if !from2.Equal(windowFrom) || !to2.Equal(windowTo) || !target2.Equal(target) || jobs != 1 {
		t.Fatalf("queued window rewritten from=%s to=%s target=%s jobs=%d", from2, to2, target2, jobs)
	}
	if hours2 != 24 || retention2 != 7 {
		t.Fatalf("source settings not applied hours=%d retention=%d", hours2, retention2)
	}

	claim, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.Apply(ctx, serviceapi.Command{CommandID: "cfg-3", Kind: "sync", InitialDays: 3, RetentionDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	var from3, to3 time.Time
	var hours3, retention3 int
	if err = store.pool.QueryRow(ctx, `SELECT j.window_from,j.window_to,s.initial_hours,s.retention_days FROM event_jobs j JOIN event_sources s USING(installation_id) WHERE j.id=$1`, jobID).Scan(&from3, &to3, &hours3, &retention3); err != nil {
		t.Fatal(err)
	}
	if !from3.Equal(windowFrom) || !to3.Equal(windowTo) || hours3 != 72 || retention3 != 14 {
		t.Fatalf("running window/settings from=%s hours=%d retention=%d", from3, hours3, retention3)
	}
	if err = store.SavePage(ctx, claim, serviceapi.EventPage{}); err != nil {
		t.Fatalf("settings attach fenced the running writer: %v", err)
	}
	_ = third

	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "cfg-stop", Kind: "disable"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "cfg-4", Kind: "sync", InitialDays: 7, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	var from4, to4, target4 time.Time
	var hours4, retention4 int
	var status string
	if err = store.pool.QueryRow(ctx, `SELECT window_from,window_to,target_to,status FROM event_jobs WHERE id=$1`, jobID).Scan(&from4, &to4, &target4, &status); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT initial_hours,retention_days FROM event_sources WHERE installation_id=$1`, installation).Scan(&hours4, &retention4); err != nil {
		t.Fatal(err)
	}
	if !from4.Equal(windowFrom) || !to4.Equal(windowTo) || !target4.Equal(target) || status != "queued" {
		t.Fatalf("enable rewound paused job from=%s to=%s target=%s status=%s", from4, to4, target4, status)
	}
	if hours4 != 168 || retention4 != 30 {
		t.Fatalf("enable snapshot hours=%d retention=%d", hours4, retention4)
	}

	for i := 0; i < 4; i++ {
		worked, err := s.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	before, err := s.Status(ctx, serviceapi.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	seedRetainedEvents(t, store, installation, 3)
	var seeded int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM crm_events WHERE installation_id=$1 AND event_id LIKE 'old-%%'`, installation).Scan(&seeded); err != nil || seeded != 3 {
		t.Fatalf("seeded=%d err=%v", seeded, err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "cfg-5", Kind: "sync", InitialDays: 2, RetentionDays: 2}); err != nil {
		t.Fatal(err)
	}
	var removed int64
	for i := 0; i < 6; i++ {
		n, err := s.Retain(ctx)
		if err != nil {
			t.Fatal(err)
		}
		removed += n
	}
	if removed < 3 {
		t.Fatalf("retention did not drop seeded events removed=%d", removed)
	}
	var remaining int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM crm_events WHERE installation_id=$1 AND event_id LIKE 'old-%%'`, installation).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("deleted events remained=%d err=%v", remaining, err)
	}
	if _, err = s.Apply(ctx, serviceapi.Command{CommandID: "cfg-6", Kind: "sync", InitialDays: 2, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM crm_events WHERE installation_id=$1 AND event_id LIKE 'old-%%'`, installation).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("raising retention restored deleted events remaining=%d", remaining)
	}
	after, err := s.Status(ctx, serviceapi.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if before.VerifiedThrough != 0 && after.VerifiedThrough != before.VerifiedThrough {
		t.Fatalf("retention/settings rewound coverage before=%+v after=%+v", before, after)
	}
	var liveFrom time.Time
	var liveJobs int
	if err = store.pool.QueryRow(ctx, `SELECT window_from,count(*) OVER() FROM event_jobs WHERE installation_id=$1 AND kind='current' AND status IN ('queued','running','retry','paused')`, installation).Scan(&liveFrom, &liveJobs); err == nil {
		if liveFrom.Before(windowFrom.Add(-time.Hour)) {
			t.Fatalf("later settings created an older window %s < %s", liveFrom, windowFrom)
		}
	}
}
