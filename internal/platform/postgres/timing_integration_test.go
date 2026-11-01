package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/requesttiming"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestRequestTimingIncludesSQLConsumptionAndFailedPoolAcquire(t *testing.T) {
	fixture := testkit.Postgres(t)
	pool, err := Open(context.Background(), fixture.Config().ConnString(), "timing-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := requesttiming.Start(context.Background(), uuid.New(), requesttiming.Core, requesttiming.Issue)
	rows, err := pool.Query(ctx, "SELECT generate_series(1,3)")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	rows.Close()
	summary := requesttiming.Snapshot(ctx)
	var sqlSeen, acquireSeen bool
	for i := 0; i < summary.Count; i++ {
		c := summary.Cells[i]
		sqlSeen = sqlSeen || c.Stage == requesttiming.SQL && c.Micros >= 10000 && c.Count == 1
		acquireSeen = acquireSeen || c.Stage == requesttiming.Acquire && c.Count == 1
	}
	if !sqlSeen || !acquireSeen || summary.Incomplete {
		t.Fatalf("SQL consumption or acquire missing: %+v", summary)
	}
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	ctx = requesttiming.Start(context.Background(), uuid.New(), requesttiming.Core, requesttiming.Validate)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := pool.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire deadline changed: %v", err)
	}
	summary = requesttiming.Snapshot(ctx)
	if summary.Count != 1 || summary.Cells[0].Stage != requesttiming.Acquire || summary.Cells[0].Outcome != requesttiming.Deadline {
		t.Fatalf("failed acquire omitted: %+v", summary)
	}
}
