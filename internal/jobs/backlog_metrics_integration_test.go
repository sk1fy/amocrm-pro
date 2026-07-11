package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestBacklogCollectorReportsExactLivePostgresState(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()

	insert := func(status string, attempts, maxAttempts int, runAfter time.Time, lockedUntil *time.Time) string {
		t.Helper()
		var id string
		var lockedBy any
		if lockedUntil != nil {
			lockedBy = "metrics-test-worker"
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO jobs (type, status, payload, attempts, max_attempts, run_after, locked_by, locked_until)
			VALUES ('metrics.test', $1, '{}'::jsonb, $2, $3, $4, $5, $6)
			RETURNING id
		`, status, attempts, maxAttempts, runAfter, lockedBy, lockedUntil).Scan(&id); err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
		return id
	}

	now := time.Now()
	readyID := insert("queued", 0, 3, now.Add(-time.Minute), nil)
	insert("retry", 1, 3, now.Add(-time.Second), nil)
	insert("queued", 3, 3, now.Add(-time.Minute), nil)
	scheduledID := insert("retry", 1, 3, now.Add(time.Hour), nil)
	liveLease := now.Add(time.Hour)
	insert("processing", 1, 3, now.Add(-time.Minute), &liveLease)
	expiredLease := now.Add(-time.Hour)
	expiredID := insert("processing", 1, 3, now.Add(-time.Minute), &expiredLease)
	insert("completed", 1, 3, now.Add(-time.Minute), nil)

	collector, err := NewBacklogCollector(pool, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	assertBacklogMetrics(t, registry, map[string]float64{
		"ready": 2, "scheduled": 1, "expired_lease": 1,
	})

	for _, mutation := range []struct {
		query string
		id    string
	}{
		{`UPDATE jobs SET status='completed', finished_at=statement_timestamp() WHERE id=$1`, readyID},
		{`UPDATE jobs SET run_after=statement_timestamp() - interval '1 second' WHERE id=$1`, scheduledID},
		{`UPDATE jobs SET status='dead', locked_by=NULL, locked_until=NULL, finished_at=statement_timestamp() WHERE id=$1`, expiredID},
	} {
		if _, err := pool.Exec(ctx, mutation.query, mutation.id); err != nil {
			t.Fatalf("mutate backlog: %v", err)
		}
	}
	assertBacklogMetrics(t, registry, map[string]float64{
		"ready": 2, "scheduled": 0, "expired_lease": 0,
	})

	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status='cancelled', finished_at=statement_timestamp()
		WHERE status IN ('queued', 'retry') AND attempts < max_attempts
	`); err != nil {
		t.Fatalf("clear runnable backlog: %v", err)
	}
	assertBacklogMetrics(t, registry, map[string]float64{
		"ready": 0, "scheduled": 0, "expired_lease": 0,
	})
}

func assertBacklogMetrics(t *testing.T, registry *prometheus.Registry, want map[string]float64) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "amocrm_jobs_backlog" {
		t.Fatalf("unexpected metric families: %#v", families)
	}
	got := make(map[string]float64)
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "kind" {
			t.Fatalf("unexpected labels: %#v", metric.Label)
		}
		got[metric.Label[0].GetValue()] = metric.GetGauge().GetValue()
	}
	for kind, count := range want {
		if got[kind] != count {
			t.Fatalf("%s backlog = %v, want %v (all: %#v)", kind, got[kind], count, got)
		}
	}
}
