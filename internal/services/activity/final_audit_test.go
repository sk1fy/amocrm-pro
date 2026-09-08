package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"strings"
	"testing"
)

func TestBudgetCardPreservesZeroAndPrecision(t *testing.T) {
	for _, tc := range []struct{ before, after, want string }{
		{`{"sale":1000}`, `{"sale":0}`, "1000 → 0"},
		{`{"sale":0}`, `{"sale":1000}`, "0 → 1000"},
		{`{"sale":0}`, `{"sale":0}`, "0 → 0"},
		{`{}`, `{"sale":0}`, "— → 0"},
		{`{"sale":null}`, `{"sale":0}`, "— → 0"},
		{`{"sale":9007199254740993.125}`, `{"sale":1e30}`, "9007199254740993.125 → 1e30"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			e := serviceapi.Event{ID: "budget", Type: "sale_field_changed", ValueBefore: json.RawMessage(`[{"sale_field_value":` + tc.before + `}]`), ValueAfter: json.RawMessage(`[{"sale_field_value":` + tc.after + `}]`)}
			s := New(&memorySettings{}, &fakePolicy{}, &fakeEvents{event: e}, &fakeGateway{})
			got, err := s.EventCard(context.Background(), serviceapi.EventRequest{Auth: serviceapi.Auth{Token: "verified"}, EventID: e.ID})
			if err != nil || got.View == nil || !strings.Contains(got.View.Summary, tc.want) {
				t.Fatalf("card %+v err=%v", got.View, err)
			}
			if string(got.ValueBefore) != string(e.ValueBefore) || string(got.ValueAfter) != string(e.ValueAfter) {
				t.Fatal("payload changed")
			}
		})
	}
	e := presentEvent(serviceapi.Event{Type: "entity_responsible_changed", ValueBefore: json.RawMessage(`[{"responsible_user":{"id":1000}}]`), ValueAfter: json.RawMessage(`[{"responsible_user":{"id":7}}]`)}, nil, true)
	if !strings.Contains(e.View.Summary, "#1000 → #7") {
		t.Fatal(e.View.Summary)
	}
}

func TestEnumCardScopesCatalogAndUnavailableFallback(t *testing.T) {
	e := serviceapi.Event{ID: "enum", Type: "custom_field_value_changed", EntityType: "lead", ValueAfter: json.RawMessage(`[{"custom_field_value":{"field_id":10,"enum_id":1}}]`)}
	catalog := json.RawMessage(`{"fields":[{"id":10,"name":"Wrong entity","entity_type":"contacts","enums":[{"id":1,"value":"Wrong label"}]}]}`)
	for _, o := range []serviceapi.EnrichmentObject{
		{ObjectKind: serviceapi.ObjectCustomField, ObjectKey: "contacts", State: serviceapi.EnrichmentReady, Payload: catalog},
		{ObjectKind: serviceapi.ObjectCustomField, ObjectKey: "leads", State: serviceapi.EnrichmentReady, Payload: catalog},
		{ObjectKind: serviceapi.ObjectCustomField, ObjectKey: "leads", State: serviceapi.EnrichmentUnavailable},
	} {
		e.Enrichment = []serviceapi.EnrichmentObject{o}
		got := presentEvent(e, nil, true)
		found := false
		for _, d := range got.View.Details {
			if strings.HasPrefix(d.Key, "custom_field_enum:") {
				found = true
				if d.Text != "#1 (название варианта недоступно)" || strings.Contains(d.Label, "Wrong") {
					t.Fatal(d)
				}
			}
		}
		if !found {
			t.Fatal("missing enum fallback")
		}
	}
}

func TestEnumCardKeepsLabelsBeyondDetailBlockLimit(t *testing.T) {
	entries := make([]string, 40)
	field := serviceapi.CustomField{ID: 10, EntityType: "leads", Name: "Варианты"}
	for i := range entries {
		id := int64(i + 1)
		entries[i] = fmt.Sprintf(`{"custom_field_value":{"field_id":10,"enum_id":%d}}`, id)
		field.Enums = append(field.Enums, serviceapi.CustomFieldEnum{ID: id, Value: fmt.Sprintf("Вариант %d", id)})
	}
	payload, _ := json.Marshal(serviceapi.CustomFieldCatalog{Fields: []serviceapi.CustomField{field}})
	event := presentEvent(serviceapi.Event{Type: "custom_field_value_changed", EntityType: "lead", ValueAfter: json.RawMessage("[" + strings.Join(entries, ",") + "]"), Enrichment: []serviceapi.EnrichmentObject{{ObjectKind: serviceapi.ObjectCustomField, ObjectKey: "leads", State: serviceapi.EnrichmentReady, Payload: payload}}}, nil, true)
	var labels strings.Builder
	for _, detail := range event.View.Details {
		if strings.HasPrefix(detail.Key, "custom_field_enum:") {
			labels.WriteString(detail.Text)
		}
	}
	for i := range entries {
		want := fmt.Sprintf("Вариант %d (#%d)", i+1, i+1)
		if !strings.Contains(labels.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if len(event.View.Details) > 32 {
		t.Fatalf("too many detail blocks: %d", len(event.View.Details))
	}
}
