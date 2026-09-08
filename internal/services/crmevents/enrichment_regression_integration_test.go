package crmevents

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestEnrichmentNegativeCacheExpires(t *testing.T) {
	for _, code := range []serviceapi.Code{serviceapi.PermissionDenied, serviceapi.NotFound} {
		t.Run(string(code), func(t *testing.T) {
			s, p, _ := setup(t)
			accepted(t, s, p)
			g := newFixtureGateway(t)
			g.notesErr = serviceapi.Fail(code, "missing")
			s.gateway = g
			g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
				return serviceapi.EventPage{Events: fixturePage(t, time.Now().Add(-time.Minute).Unix(), "note_reference")}, nil
			}
			runPages(t, s, 2)
			drainEnrichment(t, s)
			if worked, err := s.EnrichOnce(context.Background()); err != nil || worked {
				t.Fatalf("negative TTL ignored: worked=%v err=%v", worked, err)
			}
			_, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_enrichment_objects SET run_after=now()-interval '2 hours' WHERE object_kind='note'`)
			if err != nil {
				t.Fatal(err)
			}
			g.notesErr = nil
			g.notes = []serviceapi.Note{{ID: 51005, EntityID: 31001, EntityType: "leads", Params: json.RawMessage(`{"text":"restored"}`)}}
			if worked, err := s.EnrichOnce(context.Background()); err != nil || !worked {
				t.Fatalf("expired negative TTL not retried: worked=%v err=%v", worked, err)
			}
			detail, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "synthetic-note_reference"})
			if err != nil {
				t.Fatal(err)
			}
			note := requireEnrichment(t, detail, serviceapi.ObjectNote)
			if note.State != serviceapi.EnrichmentReady || !strings.Contains(string(note.Payload), "restored") {
				t.Fatalf("not restored: %+v", note)
			}
		})
	}
}

func TestEnrichmentHistoricalNoteIsolation(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	g := newFixtureGateway(t)
	s.gateway = g
	at := time.Now().Add(-time.Minute).Unix()
	g.notes = []serviceapi.Note{{ID: 51, EntityID: 31, EntityType: "leads", Params: json.RawMessage(`{"text":"current version"}`)}}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{
			{ID: "first", CreatedAt: at, CreatedBy: 7, Type: "common_note_added", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":51,"text":"first version"}}]`)},
			{ID: "second", CreatedAt: at, CreatedBy: 7, Type: "common_note_updated", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":51,"text":"second version"}}]`)},
			{ID: "reference", CreatedAt: at, CreatedBy: 7, Type: "common_note_updated", EntityType: "lead", EntityID: 31, ValueAfter: json.RawMessage(`[{"note":{"id":51}}]`)},
		}}, nil
	}
	runPages(t, s, 2)
	drainEnrichment(t, s)
	for _, id := range []string{"first", "second", "reference"} {
		e, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		note := requireEnrichment(t, e, serviceapi.ObjectNote)
		expected := id + " version"
		source := serviceapi.SourceEventPayload
		current := false
		if id == "reference" {
			expected = "current version"
			source = serviceapi.SourceNotesAPI
			current = true
		}
		if !strings.Contains(string(note.Payload), expected) || note.Source != source || note.Current != current {
			t.Fatalf("%s: %+v", id, note)
		}
	}
	// Reloading the shared current object must leave both historical versions intact.
	_, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_enrichment_objects SET run_after=now()-interval '1 hour' WHERE object_kind='note'`)
	if err != nil {
		t.Fatal(err)
	}
	g.notes[0].Params = json.RawMessage(`{"text":"new current version"}`)
	drainEnrichment(t, s)
	e, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if note := requireEnrichment(t, e, serviceapi.ObjectNote); !strings.Contains(string(note.Payload), "first version") || note.Current {
		t.Fatalf("history changed: %+v", note)
	}
}

func TestEnrichmentReadyTTLAndLargeCatalog(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	g := newFixtureGateway(t)
	s.gateway = g
	g.fields = serviceapi.CustomFieldCatalog{}
	for i := int64(1); i <= 400; i++ {
		g.fields.Fields = append(g.fields.Fields, serviceapi.CustomField{ID: i, Name: strings.Repeat("n", 80), Type: "text", EntityType: "leads"})
	}
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "fields", CreatedAt: at, CreatedBy: 7, Type: "custom_field_1_value_changed", EntityType: "lead", EntityID: 31001, ValueAfter: json.RawMessage(`[{"custom_field_value":{"field_id":1,"text":"value"}}]`)}}}, nil
	}
	runPages(t, s, 2)
	drainEnrichment(t, s)
	read := func() serviceapi.CustomFieldCatalog {
		t.Helper()
		e, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "fields"})
		if err != nil {
			t.Fatal(err)
		}
		o := requireEnrichment(t, e, serviceapi.ObjectCustomField)
		if o.State != serviceapi.EnrichmentReady {
			t.Fatalf("catalog state: %+v", o)
		}
		var got serviceapi.CustomFieldCatalog
		if err = json.Unmarshal(o.Payload, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Fields) != 400 {
			t.Fatalf("lost catalog: fields=%d", len(got.Fields))
		}
		return got
	}
	read()
	g.fields.Fields[0].Name = "Renamed"
	if worked, err := s.EnrichOnce(context.Background()); err != nil || worked {
		t.Fatalf("positive TTL ignored: %v %v", worked, err)
	}
	if read().Fields[0].Name == "Renamed" {
		t.Fatal("refreshed before TTL")
	}
	_, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_enrichment_objects SET run_after=now()-interval '1 hour' WHERE object_kind='custom_field'`)
	if err != nil {
		t.Fatal(err)
	}
	drainEnrichment(t, s)
	if read().Fields[0].Name != "Renamed" {
		t.Fatal("ready catalog not refreshed after TTL")
	}
	// Exceed the owner bound through the fake port. Never claim a truncated success.
	g.fields.Fields[0].Name = strings.Repeat("x", maxEnrichmentObjectBytes)
	_, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_enrichment_objects SET run_after=now()-interval '1 hour' WHERE object_kind='custom_field'`)
	if err != nil {
		t.Fatal(err)
	}
	drainEnrichment(t, s)
	e, err := s.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "fields"})
	if err != nil {
		t.Fatal(err)
	}
	o := requireEnrichment(t, e, serviceapi.ObjectCustomField)
	if o.State != serviceapi.EnrichmentError || o.ReasonCode != serviceapi.ReasonInvalid || len(o.Payload) != 0 {
		t.Fatalf("oversize silently accepted: %+v", o)
	}
}

func requireEnrichment(t *testing.T, e serviceapi.Event, kind string) serviceapi.EnrichmentObject {
	t.Helper()
	var found []serviceapi.EnrichmentObject
	for _, o := range e.Enrichment {
		if o.ObjectKind == kind {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s in %+v", kind, e.Enrichment)
	}
	return found[0]
}

func TestEnrichmentRefreshMigrationRepairsLegacyObjects(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: fixturePage(t, at, "note_reference", "lead_status")}, nil
	}
	runPages(t, s, 2)
	tx, err := testStore(s).pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(context.Background(), `UPDATE event_enrichment_objects SET state='ready',source=CASE WHEN object_kind='note' THEN 'event_payload' ELSE 'pipelines_api' END,payload='{"current":true}',lease_until=now()+interval '1 minute' WHERE object_kind IN ('note','pipeline')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"down", "up"} {
		script, err := os.ReadFile("../../../migrations/crmevents/000005_enrichment_refresh." + suffix + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(context.Background(), string(script)); err != nil {
			t.Fatal(err)
		}
	}
	var repaired int
	err = tx.QueryRow(context.Background(), `SELECT count(*) FROM event_enrichment_objects WHERE object_kind IN ('note','pipeline') AND state='pending' AND source='' AND payload='null'::jsonb AND fetched_at IS NULL AND lease_until IS NULL AND lease_token=1`).Scan(&repaired)
	if err != nil || repaired != 2 {
		t.Fatalf("migration repaired %d objects: %v", repaired, err)
	}
}

func TestEnrichmentNoteEnvelopePreservesMaximumParams(t *testing.T) {
	text := strings.Repeat("x", serviceapi.EnrichmentPayloadLimit-100)
	params, _ := json.Marshal(map[string]string{"text": text})
	note := serviceapi.Note{ID: 1, EntityID: 2, EntityType: "leads", Params: params}
	payload := objectPayload(note, NoteFacts(note), true)
	var got struct {
		Params struct {
			Text string `json:"text"`
		} `json:"params"`
		Facts struct {
			Text string `json:"text"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Params.Text != text || got.Facts.Text != text {
		t.Fatal("valid note params or derived facts were truncated")
	}
}
