package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/requesttiming"
	"testing"
)

func TestTaskQueueTimingDistinguishesColdWarmExpiredAndEmptyLease(t *testing.T) {
	pool, scope, _ := taskFixture(t)
	store := NewTaskQueueStore(pool)
	fetch := func(context.Context) ([]storedTask, bool, error) { return []storedTask{}, true, nil }
	run := func(state, final requesttiming.Outcome) {
		t.Helper()
		ctx := requesttiming.Start(context.Background(), uuid.New(), requesttiming.Gateway, requesttiming.OwnTasks)
		if _, err := store.Load(ctx, scope, 7, fetch); err != nil {
			t.Fatal(err)
		}
		s := requesttiming.Snapshot(ctx)
		var initialSeen, finalSeen bool
		for i := 0; i < s.Count; i++ {
			c := s.Cells[i]
			initialSeen = initialSeen || c.Stage == requesttiming.QueueInitial && c.Outcome == state
			finalSeen = finalSeen || c.Stage == requesttiming.QueueFinal && c.Outcome == final
		}
		if !initialSeen || !finalSeen || s.Incomplete {
			t.Fatalf("wrong queue evidence: %+v", s)
		}
	}
	run(requesttiming.MissingRow, requesttiming.Refreshed)
	run(requesttiming.Fresh, requesttiming.Hit)
	if _, err := pool.Exec(context.Background(), `UPDATE activity_task_queues SET fresh_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	run(requesttiming.Expired, requesttiming.Refreshed)
	if _, err := pool.Exec(context.Background(), `UPDATE activity_task_queues SET payload=NULL,fresh_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	run(requesttiming.MissingPayload, requesttiming.Refreshed)
}
