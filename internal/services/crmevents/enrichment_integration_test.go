package crmevents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type fixtureGateway struct {
	testGateway
	notesErr, tasksErr error
	notes              []serviceapi.Note
	tasks              []serviceapi.Task
	pipelines          serviceapi.PipelineCatalog
	fields             serviceapi.CustomFieldCatalog
	entities           []serviceapi.EntityName
}

func (g *fixtureGateway) Notes(context.Context, serviceapi.NotesRequest) (serviceapi.NotePage, error) {
	if g.notesErr != nil {
		return serviceapi.NotePage{}, g.notesErr
	}
	return serviceapi.NotePage{Notes: g.notes}, nil
}
func (g *fixtureGateway) Tasks(context.Context, serviceapi.TasksRequest) (serviceapi.TaskPage, error) {
	if g.tasksErr != nil {
		return serviceapi.TaskPage{}, g.tasksErr
	}
	return serviceapi.TaskPage{Tasks: g.tasks}, nil
}
func (g *fixtureGateway) Pipelines(context.Context, serviceapi.CatalogRequest) (serviceapi.PipelineCatalog, error) {
	return g.pipelines, nil
}
func (g *fixtureGateway) CustomFields(context.Context, serviceapi.CustomFieldsRequest) (serviceapi.CustomFieldCatalog, error) {
	return g.fields, nil
}
func (g *fixtureGateway) Entities(context.Context, serviceapi.EntitiesRequest) (serviceapi.EntityCatalog, error) {
	return serviceapi.EntityCatalog{Entities: g.entities}, nil
}

func TestEnrichSavePageEnqueuesAndGetEventSidecar(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	g := newFixtureGateway(t)
	s.gateway = g
	at := time.Now().Add(-time.Minute).Unix()
	events := fixturePage(t, at, "note_reference", "note_embedded", "chat_reference", "call_reference", "task_result_reference", "lead_status", "lead_added")
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: events}, nil
	}
	runPages(t, s, 2)
	store := testStore(s)
	var pending, ready, notes, tasks, pipelines, chatNotes int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE state='pending'),count(*) FILTER (WHERE state='ready'),count(*) FILTER (WHERE object_kind='note'),count(*) FILTER (WHERE object_kind='task'),count(*) FILTER (WHERE object_kind='pipeline') FROM event_enrichment_objects WHERE installation_id=$1`, p.principal.InstallationID).Scan(&pending, &ready, &notes, &tasks, &pipelines); err != nil {
		t.Fatal(err)
	}
	if pending == 0 || ready != 0 || notes == 0 || tasks == 0 || pipelines != 1 {
		t.Fatalf("enqueue pending=%d ready=%d notes=%d tasks=%d pipelines=%d", pending, ready, notes, tasks, pipelines)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM event_enrichment_links l JOIN event_enrichment_objects o USING(installation_id,object_kind,object_key) WHERE l.installation_id=$1 AND l.event_id='synthetic-chat_reference' AND o.object_kind='note'`, p.principal.InstallationID).Scan(&chatNotes); err != nil || chatNotes != 0 {
		t.Fatalf("chat enqueued notes=%d err=%v", chatNotes, err)
	}
	drainEnrichment(t, s)
	listed, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Limit: 100})
	if err != nil || len(listed.Events) == 0 {
		t.Fatalf("query %+v %v", listed, err)
	}
	for _, event := range listed.Events {
		if len(event.Enrichment) != 0 || len(event.Names) != 0 {
			t.Fatalf("list attached enrichment bodies %+v", event)
		}
	}
	detail, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "synthetic-call_reference"})
	if err != nil || len(detail.Enrichment) == 0 {
		t.Fatalf("GetEvent sidecar %+v %v", detail, err)
	}
	_, encoded, err := canonicalEvent(detail)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err = store.pool.QueryRow(context.Background(), `SELECT content_hash FROM crm_events WHERE installation_id=$1 AND event_id='synthetic-call_reference'`, p.principal.InstallationID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	if string(sum[:]) != string(stored) {
		t.Fatal("sidecar changed stored content hash")
	}
	status, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "synthetic-lead_status"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(status.Names, "pipeline_status", 81001) || !hasName(status.Names, serviceapi.ObjectEntity, 31001) {
		t.Fatalf("lead_status names %+v", status.Names)
	}
}

func TestEnrichOnceNotFoundKeepsEventQueryable(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	g := newFixtureGateway(t)
	g.notesErr = serviceapi.Fail(serviceapi.NotFound, "missing")
	s.gateway = g
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: fixturePage(t, at, "note_reference")}, nil
	}
	runPages(t, s, 2)
	drainEnrichment(t, s)
	var state, reason string
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT state,reason_code FROM event_enrichment_objects WHERE installation_id=$1 AND object_kind='note' AND object_key='51005'`, p.principal.InstallationID).Scan(&state, &reason); err != nil || state != serviceapi.EnrichmentUnavailable || reason != serviceapi.ReasonNotFound {
		t.Fatalf("404 mapping state=%s reason=%s err=%v", state, reason, err)
	}
	got, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Limit: 10})
	if err != nil || len(got.Events) != 1 || got.Events[0].ID != "synthetic-note_reference" {
		t.Fatalf("event not queryable %+v %v", got, err)
	}
	status, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || status.State == "failed" {
		t.Fatalf("collector source failed from enrichment 404: %+v %v", status, err)
	}
}

func TestEnrichCollectionSavePageWhenNotesError(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: fixturePage(t, at, "note_reference", "call_reference")}, nil
	}
	runPages(t, s, 2)
	got, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Limit: 10})
	if err != nil || len(got.Events) != 2 {
		t.Fatalf("collection stopped %+v %v", got, err)
	}
	status, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || status.State == "failed" {
		t.Fatalf("notes error failed collector %+v %v", status, err)
	}
	var count int
	if err = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM event_enrichment_objects WHERE installation_id=$1`, p.principal.InstallationID).Scan(&count); err != nil || count == 0 {
		t.Fatalf("expected enqueue despite notes stub, count=%d err=%v", count, err)
	}
}

func TestEnrichRetentionCascade(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-3 * 24 * time.Hour).Unix()
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "old-note", CreatedAt: r.From + 1, CreatedBy: 7, Type: "common_note_added", EntityID: 31001, EntityType: "lead", ValueAfter: json.RawMessage(`[{"note":{"id":51999}}]`)}}}, nil
	}
	_, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET window_from=to_timestamp($2),window_to=to_timestamp($3),target_to=to_timestamp($3) WHERE installation_id=$1`, p.principal.InstallationID, at-10, at+10)
	if err != nil {
		t.Fatal(err)
	}
	runPages(t, s, 2)
	var links int
	if err = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM event_enrichment_links WHERE installation_id=$1`, p.principal.InstallationID).Scan(&links); err != nil || links == 0 {
		t.Fatalf("links=%d err=%v", links, err)
	}
	if _, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_sources SET retention_days=2`); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Retain(context.Background())
	if err != nil || removed == 0 {
		t.Fatalf("retention removed %d: %v", removed, err)
	}
	if err = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM event_enrichment_links WHERE installation_id=$1`, p.principal.InstallationID).Scan(&links); err != nil || links != 0 {
		t.Fatalf("cascade left links=%d err=%v", links, err)
	}
}

func drainEnrichment(t *testing.T, s *Service) {
	t.Helper()
	for i := 0; i < 30; i++ {
		worked, err := s.EnrichOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("enrichment did not drain")
}

func fixturePage(t *testing.T, at int64, ids ...string) []serviceapi.Event {
	t.Helper()
	all := loadFixtureEvents(t)
	out := make([]serviceapi.Event, 0, len(ids))
	for _, id := range ids {
		event, ok := all[id]
		if !ok {
			t.Fatalf("missing fixture %s", id)
		}
		event.CreatedAt = at
		out = append(out, event)
	}
	return out
}

func newFixtureGateway(t *testing.T) *fixtureGateway {
	t.Helper()
	data, err := os.ReadFile("../../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Samples []struct {
			ID      string          `json:"sample_id"`
			Payload json.RawMessage `json:"payload"`
		} `json:"enrichment_samples"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	samples := map[string]json.RawMessage{}
	for _, sample := range fixture.Samples {
		samples[sample.ID] = sample.Payload
	}
	g := &fixtureGateway{testGateway: testGateway{events: func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }}}
	var task struct {
		ID                int64  `json:"id"`
		EntityID          int64  `json:"entity_id"`
		EntityType        string `json:"entity_type"`
		ResponsibleUserID int64  `json:"responsible_user_id"`
		Text              string `json:"text"`
		CompleteTill      int64  `json:"complete_till"`
		TaskTypeID        int64  `json:"task_type_id"`
		IsCompleted       bool   `json:"is_completed"`
		Result            struct {
			Text string `json:"text"`
		} `json:"result"`
		UpdatedAt int64 `json:"updated_at"`
	}
	if err = json.Unmarshal(samples["task_current"], &task); err != nil {
		t.Fatal(err)
	}
	g.tasks = []serviceapi.Task{{ID: task.ID, EntityID: task.EntityID, EntityType: task.EntityType, ResponsibleUserID: task.ResponsibleUserID, Text: task.Text, CompleteTill: task.CompleteTill, TaskTypeID: task.TaskTypeID, IsCompleted: task.IsCompleted, ResultText: task.Result.Text, UpdatedAt: task.UpdatedAt}}
	for _, id := range []string{"call_note_current", "common_note_current", "attachment_note_current"} {
		var note serviceapi.Note
		if err = json.Unmarshal(samples[id], &note); err != nil {
			t.Fatal(err)
		}
		note.EntityType = "leads"
		g.notes = append(g.notes, note)
	}
	g.pipelines = serviceapi.PipelineCatalog{Pipelines: []serviceapi.Pipeline{{ID: 80001, Name: "Demo A", Statuses: []serviceapi.PipelineStatus{{ID: 81001, Name: "New"}, {ID: 81002, Name: "Won"}}}, {ID: 80002, Name: "Demo B", Statuses: []serviceapi.PipelineStatus{{ID: 81002, Name: "Won"}}}}}
	g.fields = serviceapi.CustomFieldCatalog{Fields: []serviceapi.CustomField{{ID: 91001, Name: "Multi", Type: "multiselect", EntityType: "contacts"}}}
	g.entities = []serviceapi.EntityName{{ID: 31001, EntityType: "leads", Name: "Синтетическая сделка"}}
	return g
}

func hasName(names []serviceapi.CatalogName, kind string, id int64) bool {
	for _, n := range names {
		if n.Kind == kind && n.ID == id {
			return true
		}
	}
	return false
}
