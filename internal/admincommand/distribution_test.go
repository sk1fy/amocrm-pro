package admincommand

import (
	"encoding/json"
	"github.com/google/uuid"
	"testing"
)

func TestDistributionCommandActorAndStrictPreconditions(t *testing.T) {
	req := Request{TargetType: "installation", TargetID: uuid.NewString(), Command: "distribution-pause", Payload: json.RawMessage(`{"expected_paused":false}`)}
	_, _, _, first, e := normalize(req, "fixture-id", "employee:first")
	if e != nil {
		t.Fatal(e)
	}
	_, _, _, other, e := normalize(req, "fixture-id", "employee:other")
	if e != nil || first == other {
		t.Fatal("actor not included in distribution replay scope")
	}
	for _, payload := range []string{`{}`, `{"expected_paused":false,"company_id":"forged"}`, `{"expected_paused":"false"}`} {
		req.Payload = []byte(payload)
		if _, _, _, _, e = normalize(req, "fixture-id", "employee:first"); e == nil {
			t.Fatal("invalid input admitted", payload)
		}
	}
}
