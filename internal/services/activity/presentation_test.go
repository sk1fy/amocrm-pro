package activity

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestPanelPresentsLabelsAndKeepsZeroEmployees(t *testing.T) {
	events := &fakeEvents{status: serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 200}}
	events.result = serviceapi.QueryResult{
		ReadVersion: serviceapi.PresentationReadVersion,
		Events: []serviceapi.Event{{
			ID: "e1", CreatedAt: 150, CreatedBy: 7, Type: "task_completed", EntityType: "task", EntityID: 41,
			ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`),
		}},
		Summaries: []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1, LastEventAt: 150, FirstEventAt: 150}, {UserID: 71999, UniqueEvents: 1, LastEventAt: 150}},
		Status:    events.status,
		Totals:    serviceapi.QueryTotals{UniqueEvents: 2},
	}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{})
	panel, err := s.Panel(context.Background(), serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200})
	if err != nil {
		t.Fatal(err)
	}
	if panel.Coverage != serviceapi.CoverageVerified || panel.Freshness != serviceapi.FreshnessCurrent || panel.InterpretationVersion != serviceapi.InterpretationVersion {
		t.Fatalf("period state %+v", panel)
	}
	if len(panel.Users) != 3 || panel.Users[2].Name != "Пользователь #71999" {
		t.Fatalf("unknown author dropped: %+v", panel.Users)
	}
	if panel.Data.Events[0].View == nil || panel.Data.Events[0].View.Category != serviceapi.CategoryTasks || panel.Data.Events[0].View.Title != "Задача завершена" {
		t.Fatalf("view %+v", panel.Data.Events[0].View)
	}
	if panel.Data.Events[0].Type != "task_completed" {
		t.Fatal("original type replaced")
	}
}

func TestGroupFilterDoesNotQueryAllAuthors(t *testing.T) {
	events := &fakeEvents{}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{users: []serviceapi.User{{ID: 7, GroupID: 1}, {ID: 9, GroupID: 2}}})
	panel, err := s.Panel(context.Background(), serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200, GroupID: 99})
	if err != nil {
		t.Fatal(err)
	}
	if events.calls != 0 || len(panel.Users) != 0 || panel.Data.ReadVersion != serviceapi.PresentationReadVersion {
		t.Fatalf("empty group leaked into owner query: calls=%d panel=%+v", events.calls, panel)
	}
}

func TestEventCardUsesHistoricalPayloadAndLabels(t *testing.T) {
	events := &fakeEvents{event: serviceapi.Event{
		ID: "card", CreatedBy: 7, Type: "common_note_added", EntityType: "lead", EntityID: 31,
		ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"note":{"id":51}}]`),
		Enrichment: []serviceapi.EnrichmentObject{{ObjectKind: "note", ObjectKey: "51", State: serviceapi.EnrichmentPending, Source: serviceapi.SourceNotesAPI}},
	}}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{})
	event, err := s.EventCard(context.Background(), serviceapi.EventRequest{Auth: serviceapi.Auth{Token: "verified"}, EventID: "card"})
	if err != nil {
		t.Fatal(err)
	}
	if event.View == nil || event.View.Category != serviceapi.CategoryNotes || event.View.DetailState != serviceapi.DetailAvailable || event.View.EnrichmentState != serviceapi.DetailPending {
		t.Fatalf("card view %+v", event.View)
	}
	if len(event.View.Details) < 2 || string(event.ValueAfter) != string(events.event.ValueAfter) {
		t.Fatalf("historical payload lost: %+v", event)
	}
	if string(event.View.Details[0].Before) != "[]" || string(event.View.Details[1].After) != string(events.event.ValueAfter) {
		t.Fatalf("before/after sides mixed: %+v", event.View.Details)
	}
}

func TestUnknownAuthorsKeepSelectedEmployees(t *testing.T) {
	events := &fakeEvents{}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{})
	_, err := s.Panel(context.Background(), serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200, UserIDs: []int64{7}, IncludeUnknownAuthors: true})
	if err != nil {
		t.Fatal(err)
	}
	if !events.query.IncludeUnknownAuthors || len(events.query.DirectoryUserIDs) != 2 || events.query.UserIDs[0] != 7 || len(events.query.UserIDs) != 1 {
		t.Fatalf("selected users dropped: %+v", events.query)
	}
}

func TestGroupFilterClearsUnknownAuthors(t *testing.T) {
	events := &fakeEvents{}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{users: []serviceapi.User{{ID: 7, GroupID: 1, Name: "A"}, {ID: 9, GroupID: 2, Name: "B"}}})
	_, err := s.Panel(context.Background(), serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200, GroupID: 1, IncludeUnknownAuthors: true})
	if err != nil {
		t.Fatal(err)
	}
	if events.query.IncludeUnknownAuthors || events.query.GroupID != 0 || !reflect.DeepEqual(events.query.UserIDs, []int64{7}) {
		t.Fatalf("group mixed unknown authors: %+v", events.query)
	}
}

func (e *fakeEvents) GetEvent(context.Context, serviceapi.EventRequest) (serviceapi.Event, error) {
	e.calls++
	if e.event.ID == "" {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "event not found")
	}
	return e.event, nil
}

func TestPeriodStateDoesNotMarkVerifiedDayStaleForLag(t *testing.T) {
	coverage, freshness, empty := periodState(serviceapi.Query{From: 100, To: 200}, serviceapi.QueryResult{
		Status: serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 200, LagSeconds: 900},
		Totals: serviceapi.QueryTotals{UniqueEvents: 0},
	})
	if coverage != serviceapi.CoverageVerified || freshness != serviceapi.FreshnessLagging || empty != serviceapi.EmptyReasonNoEvents {
		t.Fatalf("%s %s %s", coverage, freshness, empty)
	}
}
