package crmevents

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// This is also the source of the mounted-panel regression fixture. It captures
// a real operation returned after the CRM-owned job commits, not a handwritten
// frontend example that could silently use a different terminal spelling.
func TestOperationSuccessContractPreservesCompletedStorage(t *testing.T) {
	s, p, _ := setup(t)
	op := accepted(t, s, p)
	runPages(t, s, 2)
	ctx := context.Background()
	var operationStorage, jobStorage string
	if err := testStore(s).pool.QueryRow(ctx, `SELECT o.status,j.status FROM event_operations o JOIN event_operation_jobs oj ON oj.operation_id=o.id JOIN event_jobs j ON j.id=oj.job_id WHERE o.id=$1`, op.ID).Scan(&operationStorage, &jobStorage); err != nil {
		t.Fatal(err)
	}
	if operationStorage != "completed" || jobStorage != "completed" {
		t.Fatalf("owner storage changed: operation=%s job=%s", operationStorage, jobStorage)
	}
	// Recreate the application adapter so persisted legacy spelling crosses the
	// same boundary after restart and after a replay whose first reply was lost.
	s = New(testStore(s).pool, p, s.gateway, s.cfg)
	result, err := s.Operation(ctx, serviceapi.OperationRequest{OperationID: op.ID})
	if err != nil || result.State != serviceapi.OperationSucceeded {
		t.Fatalf("operation response=%+v err=%v", result, err)
	}
	replayed, err := s.Apply(ctx, serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil || replayed != result {
		t.Fatalf("completed Apply replay=%+v want=%+v err=%v", replayed, result, err)
	}
	disabled, err := s.Apply(ctx, serviceapi.Command{CommandID: "disable-success", Kind: "disable"})
	if err != nil || disabled.State != serviceapi.OperationSucceeded {
		t.Fatalf("immediate disable result=%+v err=%v", disabled, err)
	}
	if file := os.Getenv("ACTIVITY_UI_OPERATION_FIXTURE"); file != "" {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		// Synthetic operation data contains no credentials. The host Actions
		// runner must be able to read this artifact created by the test container.
		if err := os.WriteFile(file, data, 0644); err != nil {
			t.Fatal(err)
		}
		// WriteFile preserves an existing file's mode; also support local reruns
		// over a fixture produced by the previous 0600 implementation.
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote real CRM Events operation response for the mounted-panel test")
	}
}
