package crmevents

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestCallFactsPageProjection(t *testing.T) {
	s, policy, _ := setup(t)
	accepted(t, s, policy)
	g := newFixtureGateway(t)
	s.gateway = g
	at := time.Now().Add(-time.Minute).Unix()
	g.notes = []serviceapi.Note{{ID: 51, EntityID: 31, EntityType: "leads", NoteType: "call_in", Params: json.RawMessage(`{"duration":120,"phone":"+70000000001","result":4,"link":"https://recording.invalid/private","text":"not projected"}`)}}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{
			{ID: "a-reference", CreatedAt: at, CreatedBy: 7, Type: "incoming_call", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":51}}]`)},
			{ID: "b-embedded", CreatedAt: at, CreatedBy: 7, Type: "incoming_call", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":51,"duration":0}}]`)},
			{ID: "c-unknown", CreatedAt: at, CreatedBy: 8, Type: "outgoing_call", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":52}}]`)},
			{ID: "d-other", CreatedAt: at, CreatedBy: 7, Type: "lead_added", EntityType: "lead", EntityID: 31},
		}}, nil
	}
	runPages(t, s, 2)
	ctx := context.Background()
	query := serviceapi.Query{From: at - 1, To: at + 1, Limit: 1}
	pending, err := s.Query(ctx, query)
	if err != nil || len(pending.Events) != 1 || pending.Events[0].CallFacts == nil || pending.Events[0].CallFacts.Duration != nil {
		t.Fatalf("before enrichment: %+v err=%v", pending, err)
	}
	drainEnrichment(t, s)
	// Reading pages/detail must use only owner storage, never Gateway Notes.
	s.gateway = nil
	var all []serviceapi.Event
	for {
		page, err := s.Query(ctx, query)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("paged read: %+v err=%v", page, err)
		}
		all = append(all, page.Events...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(all) != 4 {
		t.Fatalf("pagination changed: %d", len(all))
	}
	for _, event := range all {
		if len(event.Enrichment) != 0 || len(event.Names) != 0 {
			t.Fatalf("list leaked full sidecars: %+v", event)
		}
		detail, err := s.GetEvent(ctx, serviceapi.EventRequest{EventID: event.ID})
		if err != nil || !reflect.DeepEqual(detail.CallFacts, event.CallFacts) {
			t.Fatalf("detail/list mismatch %s: detail=%+v list=%+v err=%v", event.ID, detail.CallFacts, event.CallFacts, err)
		}
	}
	ref := all[0].CallFacts
	if ref == nil || ref.Duration == nil || *ref.Duration != 120 || ref.Phone == nil || *ref.Phone != "+70000000001" || ref.Result == nil || *ref.Result != "4" || !ref.Current || ref.Source != serviceapi.SourceNotesAPI {
		t.Fatalf("reference projection: %+v", ref)
	}
	if got := all[1].CallFacts; got == nil || got.Duration == nil || *got.Duration != 0 || got.Current || got.Source != serviceapi.SourceEventPayload {
		t.Fatalf("embedded zero replaced: %+v", got)
	}
	if got := all[2].CallFacts; got == nil || got.Duration != nil || got.Direction != "outgoing" || got.Source != "" {
		t.Fatalf("missing note became a missed call: %+v", got)
	}
	if all[3].CallFacts != nil {
		t.Fatal("non-call has call projection")
	}
	query.Cursor, query.Limit, query.Compact = "", 100, true
	compact, err := s.Query(ctx, query)
	if err != nil || len(compact.Events) != 4 {
		t.Fatalf("compact: %+v err=%v", compact, err)
	}
	for _, event := range compact.Events {
		if event.CallFacts != nil || len(event.ValueAfter) != 0 {
			t.Fatalf("compact read loads bodies: %+v", event)
		}
	}
	query.Compact, query.UserIDs = false, []int64{8}
	filtered, err := s.Query(ctx, query)
	if err != nil || len(filtered.Events) != 1 || filtered.Events[0].ID != "c-unknown" {
		t.Fatalf("author scope changed: %+v err=%v", filtered, err)
	}
	policy.principal.InstallationID = uuid.New()
	foreign, err := s.Query(ctx, query)
	if err != nil || len(foreign.Events) != 0 {
		t.Fatalf("foreign installation: %+v err=%v", foreign, err)
	}
}
