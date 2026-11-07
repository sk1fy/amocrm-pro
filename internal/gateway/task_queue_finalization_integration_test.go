package gateway

import (
	"context"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestTaskQueueFinalizesAfterLoadDeadline(t *testing.T) {
	pool, scope, _ := taskFixture(t)
	ctx := context.Background()
	store := NewTaskQueueStore(pool)
	tests := []struct {
		name  string
		stale bool
		fail  bool
	}{
		{name: "failure", fail: true},
		{name: "stale failure", fail: true, stale: true},
		{name: "success"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			userID := int64(7 + i)
			if tt.stale {
				_, err := store.Load(ctx, scope, userID, func(context.Context) ([]storedTask, bool, error) {
					return []storedTask{{ID: 1}}, true, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE activity_task_queues SET fresh_until=now()-interval '1 second' WHERE installation_id=$1 AND user_id=$2`, scope.InstallationID, userID); err != nil {
					t.Fatal(err)
				}
			}
			parent, cancel := context.WithCancel(ctx)
			defer cancel()
			snap, err := store.Load(parent, scope, userID, func(loadCtx context.Context) ([]storedTask, bool, error) {
				cancel()
				<-loadCtx.Done()
				if tt.fail {
					return nil, false, loadCtx.Err()
				}
				return []storedTask{{ID: 2}}, true, nil
			})
			if tt.fail && !tt.stale {
				if serviceapi.ErrorCode(err) != serviceapi.DeadlineExceeded {
					t.Errorf("failure code = %s, want deadline_exceeded", serviceapi.ErrorCode(err))
				}
			} else if err != nil {
				t.Errorf("finalization returned %v", err)
			}
			row, found, readErr := store.read(ctx, scope, userID)
			if readErr != nil || !found {
				t.Fatalf("persisted queue missing: found=%v err=%v", found, readErr)
			}
			var leased bool
			var failures int
			if err := pool.QueryRow(ctx, `SELECT lease_until IS NOT NULL,failures FROM activity_task_queues WHERE installation_id=$1 AND user_id=$2`, scope.InstallationID, userID).Scan(&leased, &failures); err != nil {
				t.Fatal(err)
			}
			if leased {
				t.Error("expired fetch left its refresh lease set")
			}
			if tt.fail {
				if row.errorCode != string(serviceapi.DeadlineExceeded) || failures != 1 || !row.retryAt.After(row.now) {
					t.Errorf("failure not persisted: code=%q failures=%d backoff=%v", row.errorCode, failures, row.retryAt.Sub(row.now))
				}
				if tt.stale && (!snap.Stale || snap.ErrorCode != string(serviceapi.DeadlineExceeded) || snap.RetryAfter <= 0 || len(snap.Tasks) != 1 || snap.Tasks[0].ID != 1) {
					t.Errorf("stale result = %+v", snap)
				}
				called := false
				again, retryErr := store.Load(ctx, scope, userID, func(context.Context) ([]storedTask, bool, error) {
					called = true
					return nil, false, nil
				})
				if called || (tt.stale && (!again.Stale || retryErr != nil)) || (!tt.stale && serviceapi.ErrorCode(retryErr) != serviceapi.DeadlineExceeded) {
					t.Errorf("backoff not served: called=%v snapshot=%+v err=%v", called, again, retryErr)
				}
			} else if row.fetchedAt == nil || !row.freshUntil.After(row.now) || snap.Stale || len(snap.Tasks) != 1 || snap.Tasks[0].ID != 2 {
				t.Errorf("success not published: row=%+v snapshot=%+v", row, snap)
			}
		})
	}
}
