package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseArgsAcceptsListInspectAndExistingCommands(t *testing.T) {
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	command, parsed, err := parseArgs([]string{"list"})
	if err != nil || command != "list" || parsed != uuid.Nil {
		t.Fatalf("list=%s %s %v", command, parsed, err)
	}
	command, parsed, err = parseArgs([]string{"inspect", id.String()})
	if err != nil || command != "inspect" || parsed != id {
		t.Fatalf("inspect=%s %s %v", command, parsed, err)
	}
	command, parsed, err = parseArgs([]string{"retry", id.String()})
	if err != nil || command != "retry" || parsed != id {
		t.Fatalf("retry=%s %s %v", command, parsed, err)
	}
	command, parsed, err = parseArgs([]string{"pilot-enable", id.String()})
	if err != nil || command != "pilot-enable" || parsed != id {
		t.Fatalf("pilot=%s %s %v", command, parsed, err)
	}
}

func TestParseArgsRejectsUnknownAndMalformed(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"list", "extra"},
		{"inspect"},
		{"retry", "not-a-uuid"},
		{"integrations-retry", uuid.New().String()},
	} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %q", args)
		} else if strings.Contains(err.Error(), "not-a-uuid") {
			t.Fatal("raw invalid UUID leaked")
		}
	}
}
