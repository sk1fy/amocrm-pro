package activity

import (
	"encoding/json"
	"strconv"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Resolve selected enum values from the existing sidecar. Identity includes
// entity type and field ID; the raw historical before/after remains untouched.
func customFieldDetails(event serviceapi.Event) []serviceapi.EventDetail {
	entityType, ok := serviceapi.CatalogEntityType(event.EntityType)
	if !ok {
		return nil
	}
	fields := map[int64]serviceapi.CustomField{}
	for _, object := range event.Enrichment {
		if object.ObjectKind != serviceapi.ObjectCustomField || object.ObjectKey != entityType || object.State != serviceapi.EnrichmentReady {
			continue
		}
		var catalog serviceapi.CustomFieldCatalog
		if json.Unmarshal(object.Payload, &catalog) != nil {
			continue
		}
		for _, field := range catalog.Fields {
			if field.EntityType != "" {
				kind, ok := serviceapi.CatalogEntityType(field.EntityType)
				if !ok || kind != entityType {
					continue
				}
			}
			fields[field.ID] = field
		}
	}
	var details []serviceapi.EventDetail
	for _, side := range []struct {
		key, label string
		raw        json.RawMessage
	}{{"before", "Было", event.ValueBefore}, {"after", "Стало", event.ValueAfter}} {
		var entries []json.RawMessage
		if json.Unmarshal(side.raw, &entries) != nil {
			continue
		}
		for _, entry := range entries {
			value := jsonObject(jsonObject(entry)["custom_field_value"])
			fieldID, enumID := jsonInt64(value["field_id"]), jsonInt64(value["enum_id"])
			if fieldID <= 0 || enumID <= 0 {
				continue
			}
			field := fields[fieldID]
			fieldLabel := "поле #" + strconv.FormatInt(fieldID, 10)
			if field.Name != "" {
				fieldLabel = field.Name + " (#" + strconv.FormatInt(fieldID, 10) + ")"
			}
			text := "#" + strconv.FormatInt(enumID, 10) + " (название варианта недоступно)"
			for _, option := range field.Enums {
				if option.ID == enumID && option.Value != "" {
					text = option.Value + " (#" + strconv.FormatInt(enumID, 10) + ")"
					break
				}
			}
			if len(details) < 16 {
				details = append(details, serviceapi.EventDetail{Key: "custom_field_enum:" + entityType + ":" + strconv.FormatInt(fieldID, 10) + ":" + strconv.FormatInt(enumID, 10) + ":" + side.key, Label: side.label + " · " + fieldLabel, Text: text, Current: true, Source: serviceapi.SourceCustomFieldsAPI})
				continue
			}
			// Bound the number of detail blocks without dropping selected labels.
			// Each overflow label retains its side, field ID and enum ID.
			if len(details) == 16 {
				details = append(details, serviceapi.EventDetail{Key: "custom_field_enum:" + entityType + ":more", Label: "Дополнительные варианты полей", Current: true, Source: serviceapi.SourceCustomFieldsAPI})
			}
			if details[16].Text != "" {
				details[16].Text += "; "
			}
			details[16].Text += side.label + " · " + fieldLabel + ": " + text
		}
	}
	return details
}

func messageDetails(event serviceapi.Event) []serviceapi.EventDetail {
	if event.Type != "incoming_chat_message" && event.Type != "outgoing_chat_message" && event.Type != "entity_direct_message" {
		return nil
	}
	direction := "Направление не указано"
	if event.Type == "incoming_chat_message" {
		direction = "Входящее сообщение"
	}
	if event.Type == "outgoing_chat_message" {
		direction = "Исходящее сообщение"
	}
	msg := nestedObject(event.ValueAfter, "message")
	details := []serviceapi.EventDetail{{Key: "message_direction", Label: "Направление", Text: direction, Source: serviceapi.SourceEventPayload}}
	for _, key := range []string{"id", "text", "source", "channel"} {
		if raw, ok := msg[key]; ok {
			details = append(details, serviceapi.EventDetail{Key: "message_" + key, Label: map[string]string{"id": "ID сообщения", "text": "Текст сообщения", "source": "Источник сообщения", "channel": "Канал"}[key], After: raw, Source: serviceapi.SourceEventPayload})
		}
	}
	if text, ok := jsonString(msg["text"]); !ok || text == "" {
		details = append(details, serviceapi.EventDetail{Key: "message_text_state", Label: "Текст сообщения", Text: "Текст отсутствует в данных события; загрузка текста чата по ID не поддерживается.", Source: serviceapi.SourceEventPayload})
	}
	if event.LinkedTalkContactID > 0 {
		details = append(details, serviceapi.EventDetail{Key: "linked_talk_contact_id", Label: "ID контакта беседы", Text: strconv.FormatInt(event.LinkedTalkContactID, 10), Source: serviceapi.SourceEventPayload})
	}
	return details
}
