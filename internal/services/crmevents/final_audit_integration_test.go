package crmevents

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
	"time"
)

func TestUpdatedEventReplacesEnrichmentLinks(t *testing.T) {
	for _, samePage := range []bool{false, true} {
		t.Run(fmt.Sprint(samePage), func(t *testing.T) {
			s, p, g := setup(t)
			accepted(t, s, p)
			at := time.Now().Add(-time.Minute).Unix()
			event := func(id string, note int) serviceapi.Event {
				return serviceapi.Event{ID: id, CreatedAt: at, CreatedBy: 7, Type: "common_note_added", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(fmt.Sprintf(`[{"note":{"id":%d}}]`, note))}
			}
			pages := [][]serviceapi.Event{{event("shared", 101), event("updated", 101)}, {event("updated", 202)}, {event("updated", 202)}}
			if samePage {
				pages = [][]serviceapi.Event{{event("shared", 101), event("updated", 101), event("updated", 202)}, {event("updated", 202)}}
			}
			g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
				return serviceapi.EventPage{Events: pages[r.Page-1], HasNext: true}, nil
			}
			runPages(t, s, len(pages))
			got, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "updated"})
			if err != nil {
				t.Fatal(err)
			}
			if intField(nestedObjects(got.ValueAfter, "note")[0], "id") != 202 {
				t.Fatal(string(got.ValueAfter))
			}
			notes := 0
			for _, o := range got.Enrichment {
				if o.ObjectKind == "note" {
					notes++
					if o.ObjectKey != "202" {
						t.Fatalf("stale note: %+v", o)
					}
				}
			}
			if notes != 1 {
				t.Fatalf("notes=%d", notes)
			}
			shared, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "shared"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, o := range shared.Enrichment {
				if o.ObjectKind == "note" && o.ObjectKey == "101" {
					found = true
				}
			}
			if !found {
				t.Fatal("shared object removed")
			}
			q, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Limit: 100})
			if err != nil || q.Totals.UniqueEvents != 2 {
				t.Fatalf("totals %+v %v", q.Totals, err)
			}
		})
	}
}

func TestUpdatedTaskResultReconcilesDependentNote(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	ctx := context.Background()
	g := &fixtureGateway{tasks: []serviceapi.Task{{ID: 41, EntityType: "leads", EntityID: 31}}, notes: []serviceapi.Note{{ID: 101, EntityType: "leads", EntityID: 31}, {ID: 202, EntityType: "leads", EntityID: 31}}}
	s.gateway = g
	note := 101
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{HasNext: true, Events: []serviceapi.Event{{ID: "task-result", Type: "task_result_added", CreatedAt: time.Now().Add(-time.Minute).Unix(), CreatedBy: 7, EntityType: "task", EntityID: 41, ValueAfter: json.RawMessage(fmt.Sprintf(`[{"note":{"id":%d}}]`, note))}}}, nil
	}
	runPages(t, s, 1)
	drainEnrichment(t, s)
	note = 202
	runPages(t, s, 1)
	drainEnrichment(t, s)
	check := func() {
		t.Helper()
		got, err := s.GetEvent(ctx, serviceapi.EventRequest{EventID: "task-result"})
		if err != nil {
			t.Fatal(err)
		}
		notes := 0
		for _, o := range got.Enrichment {
			if o.ObjectKind == "note" {
				notes++
				if o.ObjectKey != "202" {
					t.Fatalf("stale task note %+v", o)
				}
			}
		}
		if notes != 1 {
			t.Fatalf("notes=%d", notes)
		}
	}
	check()
	// A subsequent task refresh must not resurrect the previous event version.
	_, err := testStore(s).pool.Exec(ctx, `UPDATE event_enrichment_objects SET run_after=now()-interval '1 second' WHERE installation_id=$1 AND object_kind='task'`, p.principal.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	drainEnrichment(t, s)
	check()
}
