package crmevents

import (
	"context"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestCategoryFilterUnknownAuthorsAndTaskMetrics(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	seedStage2History(t, s, p.principal,
		serviceapi.Event{ID: "task-1", CreatedAt: at, CreatedBy: 7, Type: "task_completed", EntityType: "task", EntityID: 41},
		serviceapi.Event{ID: "task-2", CreatedAt: at, CreatedBy: 7, Type: "task_completed", EntityType: "task", EntityID: 41},
		serviceapi.Event{ID: "call-1", CreatedAt: at, CreatedBy: 0, Type: "incoming_call", EntityType: "lead", EntityID: 31},
		serviceapi.Event{ID: "other-1", CreatedAt: at, CreatedBy: 71999, Type: "lead_added", EntityType: "lead", EntityID: 31},
	)
	tasks, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, UserIDs: []int64{7}, Categories: []string{serviceapi.CategoryTasks}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if tasks.ReadVersion != serviceapi.PresentationReadVersion || len(tasks.Events) != 2 || tasks.Totals.UniqueEvents != 2 || tasks.Totals.TaskCompletedEvents != 2 || tasks.Totals.UniqueCompletedTasks != 1 {
		t.Fatalf("task metrics %+v", tasks)
	}
	if len(tasks.Summaries) != 1 || tasks.Summaries[0].CategoryCounts[0].Category != serviceapi.CategoryTasks {
		t.Fatalf("task summary %+v", tasks.Summaries)
	}
	unknown, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, UserIDs: []int64{7}, DirectoryUserIDs: []int64{7}, IncludeUnknownAuthors: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Totals.UniqueEvents != 4 {
		t.Fatalf("unknown authors dropped: %+v", unknown)
	}
	seen := map[int64]bool{}
	for _, summary := range unknown.Summaries {
		seen[summary.UserID] = true
	}
	if !seen[0] || !seen[71999] || !seen[7] {
		t.Fatalf("authors %+v", unknown.Summaries)
	}
	selected, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, UserIDs: []int64{7}, IncludeUnknownAuthors: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Totals.UniqueEvents != 3 {
		t.Fatalf("empty directory list leaked other authors: %+v", selected)
	}
}

func TestHourBucketsKeepCoverageAndMatchTotals(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Date(2026, 9, 8, 12, 30, 0, 0, time.UTC).Unix()
	seedStage2History(t, s, p.principal, serviceapi.Event{ID: "hour", CreatedAt: at, CreatedBy: 7, Type: "lead_added", EntityType: "lead", EntityID: 1})
	from, to := at-3600, at+3600
	result, err := s.Query(context.Background(), serviceapi.Query{From: from, To: to, UserIDs: []int64{7}, Limit: 10, Timezone: "UTC", Buckets: serviceapi.BucketHour})
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	if len(result.Timeline) == 0 || len(result.Timeline) > serviceapi.MaxHourBuckets {
		t.Fatalf("timeline %+v", result.Timeline)
	}
	for _, bucket := range result.Timeline {
		sum += bucket.Count
		if bucket.Coverage == "" || bucket.EndAt < bucket.StartAt {
			t.Fatalf("bucket %+v", bucket)
		}
	}
	if sum != result.Totals.UniqueEvents || sum != 1 {
		t.Fatalf("timeline %d totals %+v", sum, result.Totals)
	}
}
