package crmevents

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func seedStage2History(t *testing.T, s *Service, p serviceapi.Principal, events ...serviceapi.Event) {
	t.Helper()
	for _, e := range events {
		before, after := e.ValueBefore, e.ValueAfter
		if len(before) == 0 {
			before = json.RawMessage(`[]`)
		}
		if len(after) == 0 {
			after = json.RawMessage(`[]`)
		}
		_, err := testStore(s).pool.Exec(context.Background(), `INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,linked_talk_contact_id,value_before,value_after,content_hash) VALUES($1,$2,to_timestamp($3),$4,$5,$6,$7,$8,$9,$10,$11)`, p.InstallationID, e.ID, e.CreatedAt, e.CreatedBy, e.Type, e.EntityID, e.EntityType, e.LinkedTalkContactID, before, after, []byte("stage2-test"))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func stage2EventIDs(events []serviceapi.Event) []string {
	ids := make([]string, len(events))
	for i := range events {
		ids[i] = events[i].ID
	}
	return ids
}

func TestHistoryFiltersOrderAndWholePeriodSummaries(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	seedStage2History(t, s, p.principal,
		serviceapi.Event{ID: "a", CreatedAt: at, CreatedBy: 7, Type: "task_added", EntityType: "lead", EntityID: 1},
		serviceapi.Event{ID: "b", CreatedAt: at, CreatedBy: 7, Type: "future_event", EntityType: "lead", EntityID: 1},
		serviceapi.Event{ID: "c", CreatedAt: at, CreatedBy: 8, Type: "task_completed", EntityType: "lead", EntityID: 2},
		serviceapi.Event{ID: "d", CreatedAt: at, CreatedBy: 9, Type: "task_added", EntityType: "lead", EntityID: 1},
		serviceapi.Event{ID: "wrong-prefix", CreatedAt: at, CreatedBy: 7, Type: "taskXadded", EntityType: "lead", EntityID: 1},
		serviceapi.Event{ID: "wrong-type", CreatedAt: at, CreatedBy: 7, Type: "call_added", EntityType: "lead", EntityID: 1},
		serviceapi.Event{ID: "wrong-entity", CreatedAt: at, CreatedBy: 7, Type: "task_added", EntityType: "contact", EntityID: 1},
		serviceapi.Event{ID: "wrong-id", CreatedAt: at, CreatedBy: 7, Type: "task_added", EntityType: "lead", EntityID: 3},
		serviceapi.Event{ID: "too-old", CreatedAt: at - 2, CreatedBy: 7, Type: "task_added", EntityType: "lead", EntityID: 1},
	)
	base := serviceapi.Query{From: at - 1, To: at + 1, UserIDs: []int64{7, 8}, Types: []string{"future_event"}, TypePrefix: "task_", EntityType: "lead", EntityIDs: []int64{1, 2}, Limit: 2}
	expectedSummary := []serviceapi.UserSummary{
		{UserID: 7, UniqueEvents: 2, FirstEventAt: at, LastEventAt: at, EntityCount: 1, CategoryCounts: []serviceapi.CategoryCount{{Category: serviceapi.CategoryOther, Count: 1}, {Category: serviceapi.CategoryTasks, Count: 1}}},
		{UserID: 8, UniqueEvents: 1, FirstEventAt: at, LastEventAt: at, EntityCount: 1, TaskCompletedEvents: 1, UniqueCompletedTasks: 1, CategoryCounts: []serviceapi.CategoryCount{{Category: serviceapi.CategoryTasks, Count: 1}}},
	}
	for _, order := range []string{"asc", "desc"} {
		t.Run(order, func(t *testing.T) {
			q := base
			q.Order = order
			var ids []string
			for page := 0; page < 3; page++ {
				result, err := s.Query(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if result.ReadVersion != serviceapi.PresentationReadVersion || result.PayloadsOmitted || !reflect.DeepEqual(result.Summaries, expectedSummary) {
					t.Fatalf("page %d metadata: %+v", page, result)
				}
				ids = append(ids, stage2EventIDs(result.Events)...)
				if result.NextCursor == "" {
					break
				}
				q.Cursor, q.Limit = result.NextCursor, 1 // Limit may change without changing the query.
			}
			want := []string{"a", "b", "c"}
			if order == "desc" {
				want = []string{"c", "b", "a"}
			}
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("order %s: got %v want %v", order, ids, want)
			}
		})
	}
	for _, filters := range []serviceapi.Query{
		{Types: []string{"future_event"}},
		{TypePrefix: "future_"},
		{EntityType: "contact"},
	} {
		filters.From, filters.To, filters.Limit = at-1, at+1, 10
		result, err := s.Query(context.Background(), filters)
		if err != nil || len(result.Events) != 1 {
			t.Fatalf("single filter %+v: %+v %v", filters, result, err)
		}
	}
}

func TestHistoryCursorRejectsScopeAndChangedFilters(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	seedStage2History(t, s, p.principal, serviceapi.Event{ID: "a", CreatedAt: at}, serviceapi.Event{ID: "b", CreatedAt: at})
	q := serviceapi.Query{From: at - 1, To: at + 1, Limit: 1}
	result, err := s.Query(context.Background(), q)
	if err != nil || result.NextCursor == "" {
		t.Fatalf("first page %+v %v", result, err)
	}
	q.Cursor = result.NextCursor
	changed := q
	changed.Compact = true
	if _, err := s.Query(context.Background(), changed); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("changed projection: %v", err)
	}
	changed = q
	changed.TypePrefix = "future_"
	if _, err := s.Query(context.Background(), changed); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("changed filter: %v", err)
	}
	p.principal.IntegrationID = uuid.New()
	if _, err := s.Query(context.Background(), q); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("cross-integration cursor: %v", err)
	}
}

func TestCompactHistoryBoundedAndFullDetailScoped(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	// Large valid CRM fields make a full page exceed the owner response budget;
	// compact projection must not copy, truncate, or replace the stored payload.
	payload := json.RawMessage(`[{"text":"` + strings.Repeat("x", 60<<10) + `","id":9007199254740993,"unknown":{"value":null}}]`)
	var events []serviceapi.Event
	for i := 0; i < 100; i++ {
		events = append(events, serviceapi.Event{ID: fmt.Sprintf("event-%03d", i), CreatedAt: at, CreatedBy: 7, Type: "future_event", EntityType: "lead", EntityID: 1, LinkedTalkContactID: 9123, ValueBefore: payload, ValueAfter: payload})
	}
	seedStage2History(t, s, p.principal, events...)
	q := serviceapi.Query{From: at - 1, To: at + 1, Limit: 100, Compact: true}
	result, err := s.Query(context.Background(), q)
	if err != nil || len(result.Events) != 100 || !result.PayloadsOmitted || result.ReadVersion != serviceapi.PresentationReadVersion {
		t.Fatalf("compact metadata events=%d %+v %v", len(result.Events), result.Status, err)
	}
	for _, event := range result.Events {
		if event.ValueBefore != nil || event.ValueAfter != nil || event.LinkedTalkContactID != 9123 {
			t.Fatalf("compact projected unexpected data: %s", event.ID)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 64<<10 {
		t.Fatalf("compact bytes=%d err=%v", len(encoded), err)
	}
	q.Compact = false
	if _, err = s.Query(context.Background(), q); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
		t.Fatalf("oversized full page silently returned: %v", err)
	}
	event, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "event-000"})
	if err != nil || event.LinkedTalkContactID != 9123 {
		t.Fatalf("full detail %+v %v", event, err)
	}
	var normalizedWant, normalizedActual any
	wantDecoder := json.NewDecoder(strings.NewReader(string(payload)))
	wantDecoder.UseNumber()
	actualDecoder := json.NewDecoder(strings.NewReader(string(event.ValueAfter)))
	actualDecoder.UseNumber()
	if err = wantDecoder.Decode(&normalizedWant); err != nil {
		t.Fatal(err)
	}
	if err = actualDecoder.Decode(&normalizedActual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalizedActual, normalizedWant) || string(event.ValueBefore) != string(event.ValueAfter) {
		t.Fatal("full detail lost payload or numeric precision")
	}
	if g.calls != 0 {
		t.Fatalf("read unexpectedly called amoCRM %d times", g.calls)
	}
	if _, err = s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "unknown"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("unknown ID %v", err)
	}
	p.principal.IntegrationID = uuid.New()
	if _, err = s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "event-000"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("cross-integration ID %v", err)
	}
	p.principal.InstallationID = uuid.New()
	if _, err = s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "event-000"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("cross-installation ID %v", err)
	}
}

func TestHistoryPaginationIsLiveAcrossLateInsertAndDelete(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	seedStage2History(t, s, p.principal, serviceapi.Event{ID: "b", CreatedAt: at}, serviceapi.Event{ID: "d", CreatedAt: at})
	q := serviceapi.Query{From: at - 1, To: at + 1, Limit: 1}
	first, err := s.Query(context.Background(), q)
	if err != nil || !reflect.DeepEqual(stage2EventIDs(first.Events), []string{"b"}) {
		t.Fatalf("first %+v %v", first, err)
	}
	seedStage2History(t, s, p.principal, serviceapi.Event{ID: "a", CreatedAt: at}, serviceapi.Event{ID: "c", CreatedAt: at})
	// Retention may delete the cursor row; the tuple position remains valid.
	if _, err = testStore(s).pool.Exec(context.Background(), `DELETE FROM crm_events WHERE installation_id=$1 AND event_id='b'`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	q.Cursor, q.Limit = first.NextCursor, 100
	next, err := s.Query(context.Background(), q)
	if err != nil || !reflect.DeepEqual(stage2EventIDs(next.Events), []string{"c", "d"}) || len(next.Summaries) != 1 || next.Summaries[0].UniqueEvents != 3 {
		t.Fatalf("live continuation %+v %v", next, err)
	}
	q.Cursor = ""
	fresh, err := s.Query(context.Background(), q)
	if err != nil || !reflect.DeepEqual(stage2EventIDs(fresh.Events), []string{"a", "c", "d"}) {
		t.Fatalf("restart sees late insertion %+v %v", fresh, err)
	}
	if _, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "b"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("deleted detail: %v", err)
	}
}

func TestUnenabledHistoryRetainsReadContract(t *testing.T) {
	s, _, _ := setup(t)
	at := time.Now().Unix()
	result, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Compact: true})
	if err != nil || result.Status.State != "not_enabled" || result.ReadVersion != serviceapi.PresentationReadVersion || !result.PayloadsOmitted || result.Events == nil || result.Summaries == nil {
		t.Fatalf("empty read %+v %v", result, err)
	}
}
