package crmevents

import (
	"encoding/json"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestFactsCallResponsibleUnconfirmed(t *testing.T) {
	params := json.RawMessage(`{"duration":37,"source":"SyntheticPBX","link":"https://recording.example.invalid/demo-call","call_responsible":71002,"uniq":"synthetic-call-2"}`)
	facts := CallFacts("outgoing_call", 71001, params)
	if facts["direction"] != "outgoing" {
		t.Fatalf("direction %+v", facts["direction"])
	}
	if facts["duration"] != int64(37) {
		t.Fatalf("duration %+v", facts["duration"])
	}
	if facts["source"] != "SyntheticPBX" {
		t.Fatalf("source aliased or dropped %+v", facts["source"])
	}
	if _, ok := facts["src"]; ok {
		t.Fatal("src was invented from source")
	}
	if facts["created_by"] != int64(71001) {
		t.Fatalf("created_by %+v", facts["created_by"])
	}
	if facts["call_responsible"] != int64(71002) {
		t.Fatalf("call_responsible %+v", facts["call_responsible"])
	}
	if facts["attribution"] != "unconfirmed" {
		t.Fatalf("expected unconfirmed attribution, got %+v", facts["attribution"])
	}
}

func TestFactsDoNotInventChatText(t *testing.T) {
	payload := json.RawMessage(`[{"message":{"id":"00000000-0000-4000-8000-000000000001"}}]`)
	facts := ChatFacts("incoming_chat_message", payload)
	if facts["direction"] != "incoming" {
		t.Fatalf("direction %+v", facts["direction"])
	}
	if facts["message_id"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("message id %+v", facts["message_id"])
	}
	if _, ok := facts["text"]; ok {
		t.Fatalf("invented chat text %+v", facts)
	}
	candidate := json.RawMessage(`[{"message":{"id":"00000000-0000-4000-8000-000000000002","text":"Искусственный текст сообщения"}}]`)
	if _, ok := ChatFacts("outgoing_chat_message", candidate)["text"]; ok {
		t.Fatal("chat facts copied message text as if it were a chat API body")
	}
}

func TestFactsNullLinkStaysUnknown(t *testing.T) {
	params := json.RawMessage(`{"duration":0,"link":null,"src":"SyntheticTelephony","uniq":"synthetic-call-1"}`)
	facts := CallFacts("incoming_call", 71001, params)
	if facts["duration"] != int64(0) {
		t.Fatalf("present zero duration omitted: %+v", facts["duration"])
	}
	if _, ok := facts["link"]; !ok {
		t.Fatal("null link key dropped")
	}
	if facts["link"] != nil {
		t.Fatalf("null link was treated as a URL: %+v", facts["link"])
	}
	if facts["src"] != "SyntheticTelephony" {
		t.Fatalf("src %+v", facts["src"])
	}
	if _, ok := facts["source"]; ok {
		t.Fatal("source was aliased from src")
	}
	if _, ok := CallFacts("incoming_call", 71001, json.RawMessage(`{"src":"x"}`))["duration"]; ok {
		t.Fatal("missing duration stored as 0")
	}
}

func TestFactsTaskAndNoteAndChange(t *testing.T) {
	task := serviceapi.Task{ID: 41001, EntityID: 31001, EntityType: "leads", ResponsibleUserID: 71002, Text: "current", CompleteTill: 10, TaskTypeID: 2, IsCompleted: true, ResultText: "done"}
	facts := TaskFacts(task)
	if facts["current"] != true || facts["entity_id"] != int64(31001) || facts["result_text"] != "done" {
		t.Fatalf("task facts %+v", facts)
	}
	note := serviceapi.Note{NoteType: "attachment", Params: json.RawMessage(`{"file_uuid":"u","version_uuid":null,"file_name":"demonstration.txt","text":"body"}`)}
	nf := NoteFacts(note)
	if nf["note_type"] != "attachment" || nf["file_name"] != "demonstration.txt" || nf["text"] != "body" {
		t.Fatalf("note facts %+v", nf)
	}
	if nf["version_uuid"] != nil {
		t.Fatalf("null version should stay unknown %+v", nf["version_uuid"])
	}
	before := json.RawMessage(`[{"lead_status":{"id":1}}]`)
	after := json.RawMessage(`[{"lead_status":{"id":2}}]`)
	cf := ChangeFacts(before, after)
	if string(cf["value_before"].(json.RawMessage)) != string(before) || string(cf["value_after"].(json.RawMessage)) != string(after) {
		t.Fatalf("change facts mutated B/A %+v", cf)
	}
}
