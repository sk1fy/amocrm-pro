package crmevents

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestPlanFixtureCases(t *testing.T) {
	cases := loadFixtureEvents(t)
	noteRef := planEnrichment(cases["note_reference"])
	if obj := planned(t, noteRef, serviceapi.ObjectNote, "51005"); obj.State != serviceapi.EnrichmentPending || obj.ParentType != "leads" {
		t.Fatalf("note_reference %+v", obj)
	}
	noteEmb := planEnrichment(cases["note_embedded"])
	if obj := planned(t, noteEmb, serviceapi.ObjectNote, "51006"); obj.State != serviceapi.EnrichmentReady || obj.Source != serviceapi.SourceEventPayload {
		t.Fatalf("note_embedded %+v", obj)
	}
	chat := planEnrichment(cases["chat_reference"])
	if hasKind(chat, serviceapi.ObjectNote) || hasKind(chat, serviceapi.ObjectTask) {
		t.Fatalf("chat_reference enqueued notes/tasks %+v", chat)
	}
	if planned(t, chat, serviceapi.ObjectEntity, "leads:31001").Key != "leads:31001" {
		t.Fatal("chat_reference should enqueue entity name only")
	}
	call := planEnrichment(cases["call_reference"])
	if obj := planned(t, call, serviceapi.ObjectNote, "51003"); obj.State != serviceapi.EnrichmentPending || obj.ParentType != "leads" {
		t.Fatalf("call_reference %+v", obj)
	}
	result := planEnrichment(cases["task_result_reference"])
	_ = planned(t, result, serviceapi.ObjectTask, "41001")
	if note, ok := findPlanned(result, serviceapi.ObjectNote, "51001"); ok && (note.ParentType == "task" || note.ParentType == "tasks") {
		t.Fatalf("task_result_reference used task as Notes parent %+v", note)
	}
	if _, ok := findPlanned(result, serviceapi.ObjectNote, "51001"); ok {
		t.Fatal("task_result_reference enqueued a note before parent was known")
	}
	status := planEnrichment(cases["lead_status"])
	if obj := planned(t, status, serviceapi.ObjectPipeline, "leads"); obj.Kind != serviceapi.ObjectPipeline {
		t.Fatalf("lead_status %+v", obj)
	}
}

func TestCanonicalEventHashIgnoresSidecar(t *testing.T) {
	e := serviceapi.Event{ID: "legacy", CreatedAt: 1, CreatedBy: 7, Type: "lead_added", EntityID: 8, EntityType: "lead"}
	_, encoded, err := canonicalEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Enrichment = []serviceapi.EnrichmentObject{{ObjectKind: serviceapi.ObjectNote, ObjectKey: "1", State: serviceapi.EnrichmentReady, Payload: json.RawMessage(`{"text":"x"}`)}}
	e.Names = []serviceapi.CatalogName{{Kind: serviceapi.ObjectEntity, ID: 8, Name: "Lead", Current: true, State: serviceapi.EnrichmentReady}}
	_, withSidecar, err := canonicalEvent(e)
	if err != nil || string(encoded) != string(withSidecar) {
		t.Fatalf("sidecar changed canonical bytes %s vs %s", encoded, withSidecar)
	}
}

func TestEnrichmentErrorMapping(t *testing.T) {
	state, reason, delay := enrichmentFailure(serviceapi.Fail(serviceapi.NotFound, "gone"), 1, 5)
	if state != serviceapi.EnrichmentUnavailable || reason != serviceapi.ReasonNotFound || delay != 15*time.Minute {
		t.Fatalf("not_found %s %s %s", state, reason, delay)
	}
	state, reason, delay = enrichmentFailure(serviceapi.Fail(serviceapi.PermissionDenied, "no"), 1, 5)
	if state != serviceapi.EnrichmentUnavailable || reason != serviceapi.ReasonPermissionDenied || delay != time.Hour {
		t.Fatalf("permission %s %s %s", state, reason, delay)
	}
	state, reason, delay = enrichmentFailure(serviceapi.Fail(serviceapi.Unavailable, "down"), 1, 5)
	if state != serviceapi.EnrichmentRetry || reason != serviceapi.ReasonTemporary || delay != 2*time.Second {
		t.Fatalf("unavailable %s %s %s", state, reason, delay)
	}
	state, reason, delay = enrichmentFailure(serviceapi.Fail(serviceapi.Unavailable, "down"), 5, 5)
	if state != serviceapi.EnrichmentRetry || reason != serviceapi.ReasonTemporary || delay != 10*time.Minute {
		t.Fatalf("exhausted %s %s", state, reason)
	}
	state, reason, _ = enrichmentFailure(serviceapi.Fail(serviceapi.ReauthRequired, "reauth"), 1, 5)
	if state != serviceapi.EnrichmentRetry || reason != serviceapi.ReasonTemporary {
		t.Fatalf("reauth %s %s", state, reason)
	}
}

func TestEnrichOnceDoesNotFailCollectorSource(t *testing.T) {
	repo := &enrichmentRepo{claim: EnrichmentClaim{ID: uuid.New(), InstallationID: uuid.New(), IntegrationID: uuid.New(), Kind: serviceapi.ObjectNote, ParentType: "leads", Objects: []claimedObject{{Key: "51005", ParentType: "leads", ParentID: 31001, ObjectID: 51005, Token: 1}}}}
	g := &testGateway{events: func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }}
	cfg := DefaultConfig()
	cfg.Logger = slog.New(slog.DiscardHandler)
	svc := NewWithRepository(repo, &testPolicy{}, g, cfg)
	worked, err := svc.EnrichOnce(context.Background())
	if err != nil || !worked || repo.failed == nil {
		t.Fatalf("worked=%t err=%v failed=%v", worked, err, repo.failed)
	}
	if serviceapi.ErrorCode(repo.failed) != serviceapi.Unavailable {
		t.Fatalf("gateway miss mapped incorrectly: %v", repo.failed)
	}
	if repo.sourceFailed {
		t.Fatal("enrichment failure marked collector source failed")
	}
}

type enrichmentRepo struct {
	Repository
	claim        EnrichmentClaim
	failed       error
	sourceFailed bool
	saved        []enrichmentSave
}

func (r *enrichmentRepo) ClaimEnrichment(context.Context) (EnrichmentClaim, error) {
	return r.claim, nil
}
func (r *enrichmentRepo) SaveEnrichment(_ context.Context, _ EnrichmentClaim, results []enrichmentSave) error {
	r.saved = results
	return nil
}
func (r *enrichmentRepo) FailEnrichment(_ context.Context, _ EnrichmentClaim, cause error) error {
	r.failed = cause
	return nil
}
func (r *enrichmentRepo) Fail(context.Context, Slice, error) error {
	r.sourceFailed = true
	return nil
}

func loadFixtureEvents(t *testing.T) map[string]serviceapi.Event {
	t.Helper()
	data, err := os.ReadFile("../../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID    string          `json:"case_id"`
			Input json.RawMessage `json:"input"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	out := map[string]serviceapi.Event{}
	for _, entry := range fixture.Cases {
		var event serviceapi.Event
		if err = json.Unmarshal(entry.Input, &event); err != nil {
			t.Fatal(err)
		}
		out[entry.ID] = event
	}
	return out
}

func planned(t *testing.T, objects []plannedObject, kind, key string) plannedObject {
	t.Helper()
	obj, ok := findPlanned(objects, kind, key)
	if !ok {
		t.Fatalf("missing %s %s in %+v", kind, key, objects)
	}
	return obj
}

func findPlanned(objects []plannedObject, kind, key string) (plannedObject, bool) {
	for _, o := range objects {
		if o.Kind == kind && o.Key == key {
			return o, true
		}
	}
	return plannedObject{}, false
}

func hasKind(objects []plannedObject, kind string) bool {
	for _, o := range objects {
		if o.Kind == kind {
			return true
		}
	}
	return false
}
