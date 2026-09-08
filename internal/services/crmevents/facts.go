package crmevents

import (
	"bytes"
	"encoding/json"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// CallFacts extracts telephony details from note params or an embedded event
// note object. Missing duration is omitted rather than stored as 0. source and
// src stay distinct keys. A null link is stored as unknown (nil), not fetched.
func CallFacts(eventType string, createdBy int64, params json.RawMessage) map[string]any {
	facts := map[string]any{}
	switch eventType {
	case "incoming_call", "call_in":
		facts["direction"] = "incoming"
	case "outgoing_call", "call_out":
		facts["direction"] = "outgoing"
	}
	fields := jsonObject(params)
	if raw, ok := fields["duration"]; ok {
		if n, ok := jsonInt(raw); ok {
			facts["duration"] = n
		}
	}
	for _, key := range []string{"source", "src", "link", "phone", "uniq", "call_responsible", "created_by", "result", "text"} {
		if raw, ok := fields[key]; ok {
			facts[key] = jsonValue(raw)
		}
	}
	if _, ok := facts["created_by"]; !ok && createdBy != 0 {
		facts["created_by"] = createdBy
	}
	responsible, hasResponsible := intFact(facts["call_responsible"])
	author, hasAuthor := intFact(facts["created_by"])
	if !hasAuthor {
		author, hasAuthor = createdBy, createdBy != 0
	}
	if hasResponsible && hasAuthor && responsible != author {
		facts["attribution"] = "unconfirmed"
	}
	return facts
}

// TaskFacts projects a current Task read. Empty historical B/A stay on the event.
func TaskFacts(task serviceapi.Task) map[string]any {
	facts := map[string]any{
		"text":          task.Text,
		"complete_till": task.CompleteTill,
		"task_type_id":  task.TaskTypeID,
		"responsible":   task.ResponsibleUserID,
		"is_completed":  task.IsCompleted,
		"current":       true,
	}
	if task.EntityType != "" {
		facts["entity_type"] = task.EntityType
	}
	if task.EntityID != 0 {
		facts["entity_id"] = task.EntityID
	}
	if task.ResultText != "" {
		facts["result_text"] = task.ResultText
	}
	return facts
}

// NoteFacts copies note_type and params text/service/attachment identifiers.
func NoteFacts(note serviceapi.Note) map[string]any {
	facts := map[string]any{}
	if note.NoteType != "" {
		facts["note_type"] = note.NoteType
	}
	fields := jsonObject(note.Params)
	if len(fields) == 0 {
		fields = jsonObject(noteAsObject(note))
	}
	for _, key := range []string{"text", "service", "file_uuid", "version_uuid", "file_name"} {
		if raw, ok := fields[key]; ok {
			facts[key] = jsonValue(raw)
		}
	}
	return facts
}

// ChangeFacts passes historical B/A through. Catalog names must not be mixed in.
func ChangeFacts(before, after json.RawMessage) map[string]any {
	facts := map[string]any{}
	if len(before) > 0 {
		facts["value_before"] = json.RawMessage(bytes.Clone(before))
	}
	if len(after) > 0 {
		facts["value_after"] = json.RawMessage(bytes.Clone(after))
	}
	return facts
}

// ChatFacts records direction and message id. Chat body is never invented.
func ChatFacts(eventType string, payload json.RawMessage) map[string]any {
	facts := map[string]any{}
	switch eventType {
	case "incoming_chat_message":
		facts["direction"] = "incoming"
	case "outgoing_chat_message":
		facts["direction"] = "outgoing"
	}
	for _, msg := range nestedObjects(payload, "message") {
		fields := jsonObject(msg)
		if raw, ok := fields["id"]; ok {
			facts["message_id"] = jsonValue(raw)
		}
	}
	return facts
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

func jsonValue(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if n, ok := jsonInt(raw); ok {
		return n
	}
	if string(raw) == "true" {
		return true
	}
	if string(raw) == "false" {
		return false
	}
	var s string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return json.RawMessage(bytes.Clone(raw))
}

func jsonInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	if bytes.ContainsAny(raw, ".eE") {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

func intFact(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

func noteAsObject(note serviceapi.Note) json.RawMessage {
	b, err := json.Marshal(note)
	if err != nil {
		return nil
	}
	return b
}
