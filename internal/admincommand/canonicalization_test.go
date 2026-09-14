package admincommand

import (
	"encoding/json"
	"fmt"
	"testing"
)

func canonicalizationRequest(payload string) Request {
	return Request{
		TargetType: "installation",
		TargetID:   "00000000-0000-4000-8000-000000000001",
		Command:    "lead-status-configure",
		Payload:    json.RawMessage(payload),
	}
}

func TestNormalizePreservesInt64AndDistinctHashes(t *testing.T) {
	var previous [32]byte
	for index, revision := range []int64{
		9007199254740992, 9007199254740993,
		9223372036854775806, 9223372036854775807,
	} {
		t.Run(fmt.Sprint(revision), func(t *testing.T) {
			payload := fmt.Sprintf(`{"enabled":true,"expected_revision":%d}`, revision)
			req, parsed, _, hash, err := normalize(canonicalizationRequest(payload), "fixture-precise-key", "employee:test")
			if err != nil {
				t.Fatal(err)
			}
			if parsed.ExpectedRevision == nil || *parsed.ExpectedRevision != revision {
				t.Fatalf("parsed revision = %v", parsed.ExpectedRevision)
			}
			var canonical struct {
				ExpectedRevision int64 `json:"expected_revision"`
			}
			if err := json.Unmarshal(req.Payload, &canonical); err != nil {
				t.Fatalf("canonical payload no longer fits int64: %v", err)
			}
			if canonical.ExpectedRevision != revision {
				t.Fatalf("canonical revision = %d, want %d", canonical.ExpectedRevision, revision)
			}
			if index > 0 && hash == previous {
				t.Fatal("different integer payloads share an idempotency hash")
			}
			previous = hash
		})
	}
}

func TestNormalizeCanonicalizesOrderWithoutRounding(t *testing.T) {
	first := canonicalizationRequest(`{"enabled":true,"expected_revision":9007199254740993}`)
	second := canonicalizationRequest("{\n  \"expected_revision\": 9007199254740993, \"enabled\": true\n}")
	_, _, keyA, hashA, err := normalize(first, "fixture-order-key", "employee:first")
	if err != nil {
		t.Fatal(err)
	}
	_, _, keyB, hashB, err := normalize(second, "fixture-order-key", "employee:second")
	if err != nil {
		t.Fatal(err)
	}
	if keyA != keyB || hashA != hashB {
		t.Fatal("whitespace, property order or employee changed replay identity")
	}
}
