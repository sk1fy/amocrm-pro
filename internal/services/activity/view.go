package activity

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func presentEvent(event serviceapi.Event, users []serviceapi.User, card bool) serviceapi.Event {
	view := serviceapi.EventView{
		Category:        serviceapi.EventCategory(event.Type),
		Title:           serviceapi.EventTitle(event.Type),
		AuthorLabel:     serviceapi.AuthorLabel(event.CreatedBy, users),
		EntityLabel:     serviceapi.EntityLabel(event.EntityType, event.EntityID, event.Names),
		DetailState:     detailState(event),
		EnrichmentState: enrichmentState(event),
	}
	view.Summary = eventSummary(event, view)
	if card {
		view.Details = eventDetails(event)
	}
	event.View = &view
	return event
}

func presentEvents(events []serviceapi.Event, users []serviceapi.User) []serviceapi.Event {
	for i := range events {
		events[i] = presentEvent(events[i], users, false)
	}
	return events
}

func detailState(event serviceapi.Event) string {
	if len(event.ValueBefore) == 0 && len(event.ValueAfter) == 0 {
		return serviceapi.DetailOmitted
	}
	if bytes.Equal(event.ValueBefore, []byte("null")) && bytes.Equal(event.ValueAfter, []byte("null")) {
		return serviceapi.DetailAvailable
	}
	return serviceapi.DetailAvailable
}

func enrichmentState(event serviceapi.Event) string {
	if len(event.Enrichment) == 0 {
		return ""
	}
	states := map[string]int{}
	for _, object := range event.Enrichment {
		states[object.State]++
	}
	switch {
	case states[serviceapi.EnrichmentPending] > 0 && len(states) == 1:
		return serviceapi.DetailPending
	case states[serviceapi.EnrichmentUnavailable] > 0 && len(states) == 1:
		return serviceapi.DetailUnavailable
	case states[serviceapi.EnrichmentReady] > 0 && len(states) == 1:
		return serviceapi.DetailAvailable
	case len(states) > 1:
		return serviceapi.DetailMixed
	default:
		return event.Enrichment[0].State
	}
}

func eventSummary(event serviceapi.Event, view serviceapi.EventView) string {
	parts := []string{view.Title}
	if view.EntityLabel != "" {
		parts = append(parts, view.EntityLabel)
	}
	switch view.Category {
	case serviceapi.CategoryStatus:
		if change := leadStatusChange(event); change != "" {
			parts = append(parts, change)
		}
	case serviceapi.CategoryBudget:
		if change := scalarChange(event, "sale_field_value", "sale"); change != "" {
			parts = append(parts, change)
		}
	case serviceapi.CategoryResponsible:
		if change := scalarChange(event, "responsible_user", "id"); change != "" {
			parts = append(parts, change)
		}
	case serviceapi.CategoryNotes, serviceapi.CategoryCalls, serviceapi.CategoryAttachments, serviceapi.CategoryMessages:
		if id := nestedID(event.ValueAfter, "note", "id"); id != 0 {
			parts = append(parts, "примечание #"+strconv.FormatInt(id, 10))
		}
	case serviceapi.CategoryRelations:
		if label := linkLabel(event); label != "" {
			parts = append(parts, label)
		}
	case serviceapi.CategoryCustomFields:
		if id := nestedID(event.ValueAfter, "custom_field_value", "field_id"); id != 0 {
			parts = append(parts, "поле #"+strconv.FormatInt(id, 10))
		}
	}
	return strings.Join(parts, " · ")
}

func eventDetails(event serviceapi.Event) []serviceapi.EventDetail {
	var details []serviceapi.EventDetail
	if len(event.ValueBefore) > 0 && !bytes.Equal(event.ValueBefore, []byte("null")) {
		details = append(details, serviceapi.EventDetail{Key: "value_before", Label: "Было", Before: bytes.Clone(event.ValueBefore), Source: serviceapi.SourceEventPayload})
	}
	if len(event.ValueAfter) > 0 && !bytes.Equal(event.ValueAfter, []byte("null")) {
		details = append(details, serviceapi.EventDetail{Key: "value_after", Label: "Стало", After: bytes.Clone(event.ValueAfter), Source: serviceapi.SourceEventPayload})
	}
	for _, object := range event.Enrichment {
		detail := serviceapi.EventDetail{Key: object.ObjectKind + ":" + object.ObjectKey, Label: object.ObjectKind, Source: object.Source, Current: object.Current, After: bytes.Clone(object.Payload)}
		if object.State != "" && object.State != serviceapi.EnrichmentReady {
			detail.Text = object.State
			if object.ReasonCode != "" {
				detail.Text += "/" + object.ReasonCode
			}
		}
		details = append(details, detail)
	}
	if len(details) > 32 {
		details = details[:32]
	}
	return details
}

func leadStatusChange(event serviceapi.Event) string {
	before := nestedObject(event.ValueBefore, "lead_status")
	after := nestedObject(event.ValueAfter, "lead_status")
	if before == nil && after == nil {
		return ""
	}
	return formatID(before["id"]) + " → " + formatID(after["id"])
}

func scalarChange(event serviceapi.Event, object, field string) string {
	before := nestedObject(event.ValueBefore, object)
	after := nestedObject(event.ValueAfter, object)
	if before == nil && after == nil {
		return ""
	}
	return formatID(before[field]) + " → " + formatID(after[field])
}

func linkLabel(event serviceapi.Event) string {
	payload := event.ValueAfter
	if len(payload) == 0 || bytes.Equal(payload, []byte("[]")) {
		payload = event.ValueBefore
	}
	link := nestedObject(payload, "link")
	if link == nil {
		link = nestedObject(payload, "unlink")
	}
	entity := jsonObject(link["entity"])
	if entity == nil {
		return ""
	}
	kind, _ := jsonString(entity["type"])
	return serviceapi.EntityLabel(kind, jsonInt64(entity["id"]), nil)
}

func nestedID(raw json.RawMessage, object, field string) int64 {
	return jsonInt64(nestedObject(raw, object)[field])
}

func nestedObject(raw json.RawMessage, key string) map[string]json.RawMessage {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		for _, item := range items {
			fields := jsonObject(item)
			if nested := jsonObject(fields[key]); nested != nil {
				return nested
			}
		}
		return nil
	}
	return jsonObject(jsonObject(raw)[key])
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func jsonInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ = strconv.ParseInt(s, 10, 64)
		return n
	}
	return 0
}

func formatID(raw json.RawMessage) string {
	if n := jsonInt64(raw); n != 0 {
		return "#" + strconv.FormatInt(n, 10)
	}
	if s, ok := jsonString(raw); ok && s != "" {
		return s
	}
	return "—"
}
