package webhook

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestParseMultipleEvents(t *testing.T) {
	installationID := uuid.MustParse("4c261999-0990-4b36-a542-4f80d8de7af0")
	raw := []byte("account%5Bid%5D=987654&leads%5Bupdate%5D%5B0%5D%5Bid%5D=123&leads%5Bupdate%5D%5B0%5D%5Blast_modified%5D=1710000000&contacts%5Badd%5D%5B0%5D%5Bid%5D=456")

	events, err := Parse(installationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EntityType != "contacts" || events[1].EntityType != "leads" {
		t.Fatalf("events are not deterministically ordered: %#v", events)
	}
	if events[1].EntityID == nil || *events[1].EntityID != 123 {
		t.Fatalf("unexpected lead id: %#v", events[1].EntityID)
	}
}

func TestDeduplicationKeyIsStable(t *testing.T) {
	installationID := uuid.MustParse("4c261999-0990-4b36-a542-4f80d8de7af0")
	first, err := Parse(installationID, []byte("account[id]=1&leads[update][0][id]=123&leads[update][0][last_modified]=1710000000"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse(installationID, []byte("leads[update][0][last_modified]=1710000000&leads[update][0][id]=123&account[id]=1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first[0].DeduplicationKey, second[0].DeduplicationKey) {
		t.Fatal("equivalent payloads must have the same deduplication key")
	}
}

func TestAccountID(t *testing.T) {
	id, ok := AccountID([]byte("account[id]=987654"))
	if !ok || id != 987654 {
		t.Fatalf("unexpected account id: %d, %t", id, ok)
	}
}

func TestParseIgnoresUnsupportedEvents(t *testing.T) {
	events, err := ParseAllowed(uuid.New(), []byte("account[id]=1&leads[update][0][id]=123&tasks[add][0][id]=456"), []string{"update_lead"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EntityType != "leads" {
		t.Fatalf("unexpected events: %#v", events)
	}

	events, err = ParseAllowed(uuid.New(), []byte("account[id]=1&tasks[add][0][id]=456"), []string{"update_lead"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected unsupported payload to be ignored, got %#v", events)
	}
}
