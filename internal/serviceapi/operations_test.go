package serviceapi

import "testing"

func TestCanonicalOperationKeepsOneSuccessSpelling(t *testing.T) {
	for _, state := range []string{OperationAccepted, OperationRunning, OperationRetry, OperationPaused, OperationFailed, OperationSucceeded, "completed"} {
		op := Operation{ID: "id", CommandID: "command", State: state, Processed: 3, Inserted: 1, Deduplicated: 2}
		got, err := CanonicalOperation(op)
		if state == "completed" {
			op.State = OperationSucceeded
		}
		if err != nil || got != op {
			t.Fatalf("state %s: %+v %v", state, got, err)
		}
	}
	for _, state := range []string{"", "queued", "dead", "unknown"} {
		if _, err := CanonicalOperation(Operation{State: state}); ErrorCode(err) != Internal {
			t.Fatalf("uncontracted %q: %v", state, err)
		}
	}
}
