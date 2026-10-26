package servicerpc

import (
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

func TestProblemSnapshotRefreshMetadataRoundTrip(t *testing.T) {
	for _, snapshot := range []serviceapi.ProblemSnapshot{
		{Collecting: true, State: "leads", RefreshAfter: 123, Counts: []serviceapi.ProblemCount{}},
		{Collecting: true, State: "tasks", RefreshAfter: 456, SnapshotAt: 100, Stale: true, ErrorCode: "resource_exhausted", Counts: []serviceapi.ProblemCount{{EmployeeID: 7, LeadsNoTasks: 82, LeadsOverdueTasks: 108}}},
		{State: "idle", RefreshAfter: 1420, SnapshotAt: 1000, Counts: []serviceapi.ProblemCount{{EmployeeID: 7}}},
	} {
		got := fromProblemSnapshot(toProblemSnapshot(snapshot))
		if got.Collecting != snapshot.Collecting || got.RefreshAfter != snapshot.RefreshAfter || got.SnapshotAt != snapshot.SnapshotAt || got.State != snapshot.State || got.Stale != snapshot.Stale || got.ErrorCode != snapshot.ErrorCode || len(got.Counts) != len(snapshot.Counts) {
			t.Fatalf("lost refresh metadata: %+v -> %+v", snapshot, got)
		}
		if got.Counts == nil {
			t.Fatal("initial snapshot counts must encode as an empty JSON array")
		}
		if len(got.Counts) > 0 && got.Counts[0] != snapshot.Counts[0] {
			t.Fatal("lost old counts")
		}
	}
}
