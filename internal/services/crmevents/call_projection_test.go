package crmevents

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestCallProjectionUnknownZeroAndAllowlist(t *testing.T) {
	for _, duration := range []string{"null", "-1", "1.5", `"20"`, "0", "20"} {
		t.Run(duration, func(t *testing.T) {
			event := serviceapi.Event{Type: "incoming_call", ValueAfter: json.RawMessage(`[{"note":{"id":51,"params":{"duration":` + duration + `,"phone":"+70000000001","result":4,"link":"https://recording.invalid/private","text":"private"}}}]`)}
			got := projectEmbeddedCall(event)
			known := duration == "0" || duration == "20"
			if (got.Duration != nil) != known || got.Direction != "incoming" || got.Current || got.Source != serviceapi.SourceEventPayload {
				t.Fatalf("wrong null/zero/source semantics: %+v", got)
			}
			if known && *got.Duration != intField(json.RawMessage(`{"n":`+duration+`}`), "n") {
				t.Fatalf("wrong duration: %+v", got)
			}
			if got.Phone == nil || *got.Phone != "+70000000001" || got.Result == nil || *got.Result != "4" {
				t.Fatalf("lost scalar facts: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "link") {
				t.Fatalf("non-allowlisted data: %s", encoded)
			}
		})
	}
	if projectEmbeddedCall(serviceapi.Event{Type: "task_completed"}) != nil {
		t.Fatal("non-call received call_facts")
	}
	unknown := projectEmbeddedCall(serviceapi.Event{Type: "outgoing_call", ValueAfter: json.RawMessage(`[{"note":{"id":51}}]`)})
	if unknown == nil || unknown.Duration != nil || unknown.Phone != nil || unknown.Direction != "outgoing" || unknown.Source != "" || unknown.Current {
		t.Fatalf("reference-only call must be explicitly unknown: %+v", unknown)
	}
}

func TestCallProjectionDetailPrecedenceAndCanonicalHash(t *testing.T) {
	event := serviceapi.Event{ID: "synthetic", Type: "incoming_call", ValueAfter: json.RawMessage(`[{"note":{"id":51,"duration":0}}]`)}
	_, before, err := canonicalEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	objects := []serviceapi.EnrichmentObject{{ObjectKind: serviceapi.ObjectNote, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceNotesAPI, Payload: json.RawMessage(`{"facts":{"duration":90,"phone":"+70000000001"}}`)}}
	detail := attachSidecar(event, objects)
	if detail.CallFacts == nil || detail.CallFacts.Duration == nil || *detail.CallFacts.Duration != 0 || detail.CallFacts.Current {
		t.Fatalf("current Notes replaced historical zero: %+v", detail.CallFacts)
	}
	_, after, err := canonicalEvent(detail)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("projection changed stored content: before=%s after=%s err=%v", before, after, err)
	}
	event.ValueAfter = json.RawMessage(`[{"note":{"id":51}}]`)
	detail = attachSidecar(event, objects)
	if detail.CallFacts == nil || detail.CallFacts.Duration == nil || *detail.CallFacts.Duration != 90 || !detail.CallFacts.Current {
		t.Fatalf("reference-only detail did not use Notes: %+v", detail.CallFacts)
	}
	objects[0].State = serviceapi.EnrichmentPending
	if got := attachSidecar(event, objects).CallFacts; got == nil || got.Duration != nil || got.Source != "" {
		t.Fatalf("pending body became a fact: %+v", got)
	}
}
