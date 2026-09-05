package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestExecutionMetricsBoundUnknownJobCardinality(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := NewMetrics(registry)
	for i := range 1000 {
		metrics.observe(fmt.Sprintf("workflow.untrusted.%d", i), StatusFailed, time.Second)
	}
	metrics.observe("workflow.lead.set_status", StatusCompleted, 2*time.Second)
	metrics.observe("widget.ping", StatusCompleted, 3*time.Second)
	metrics.observe("webhook.parse", StatusRetry, 4*time.Second)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		switch family.GetName() {
		case "amocrm_jobs_execution_duration_seconds":
			if len(family.Metric) != 4 {
				t.Fatalf("execution series=%d", len(family.Metric))
			}
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["service"] == "other" && metric.GetHistogram().GetSampleCount() != 1000 {
					t.Fatal("unknown jobs did not aggregate")
				}
				if len(labels) != 2 {
					t.Fatalf("unexpected metric labels: %v", labels)
				}
			}
		case "amocrm_workflow_jobs_total", "amocrm_workflow_duration_seconds":
			if len(family.Metric) != 2 {
				t.Fatalf("unbounded workflow metrics: %d", len(family.Metric))
			}
		}
	}
}

func TestServiceBacklogCollectorValidation(t *testing.T) {
	if _, err := NewServiceBacklogCollector(nil, time.Second); err == nil {
		t.Fatal("accepted nil queryer")
	}
	if _, err := NewServiceBacklogCollector(errorQueryer{}, 0); err == nil {
		t.Fatal("accepted zero timeout")
	}
	collector, err := NewServiceBacklogCollector(errorQueryer{err: errors.New("unavailable")}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	if _, err := registry.Gather(); err == nil {
		t.Fatal("scrape failure not surfaced")
	}
}

func TestServiceBacklogCollectorExactCountsAndOldestReady(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO jobs(type,payload,status,attempts,max_attempts,run_after,locked_by,locked_until)
        VALUES
        ('workflow.lead.set_status','{}','queued',0,3,now()-interval '60 seconds',NULL,NULL),
        ('workflow.lead.status_transition','{}','retry',1,3,now()-interval '30 seconds',NULL,NULL),
        ('workflow.lead.set_status','{}','queued',3,3,now()-interval '1 day',NULL,NULL),
        ('workflow.lead.set_status','{}','retry',1,3,now()+interval '1 hour',NULL,NULL),
        ('webhook.parse','{}','processing',1,3,now(),'worker',now()+interval '1 hour'),
        ('widget.ping','{}','processing',1,3,now(),'worker',now()-interval '1 second');
        INSERT INTO jobs(type,payload,run_after)
        SELECT 'unknown.'||series,'{}',now()-interval '10 seconds' FROM generate_series(1,1000) series`); err != nil {
		t.Fatal(err)
	}
	collector, err := NewServiceBacklogCollector(pool, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	check := func(cleared bool) {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 2 {
			t.Fatalf("families=%d", len(families))
		}
		for _, family := range families {
			wantSeries := 12
			if family.GetName() == "amocrm_jobs_service_oldest_ready_seconds" {
				wantSeries = 3
			}
			if len(family.Metric) != wantSeries {
				t.Fatalf("%s series=%d", family.GetName(), len(family.Metric))
			}
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				got := metric.GetGauge().GetValue()
				service, kind := labels["service"], labels["kind"]
				if cleared {
					if got != 0 {
						t.Fatalf("stale metric: %s %v=%v", family.GetName(), labels, got)
					}
					continue
				}
				if kind == "" {
					switch service {
					case "lead-status":
						if got < 60 || got > 65 {
							t.Fatalf("oldest ready=%v,want ~60s", got)
						}
					case "platform":
						if got != 0 {
							t.Fatalf("platform oldest=%v", got)
						}
					case "other":
						if got < 10 || got > 15 {
							t.Fatalf("other oldest=%v", got)
						}
					}
					continue
				}
				want := float64(0)
				switch service + ":" + kind {
				case "lead-status:ready":
					want = 2
				case "lead-status:scheduled", "platform:processing", "platform:expired_lease":
					want = 1
				case "other:ready":
					want = 1000
				}
				if got != want {
					t.Fatalf("%s:%s=%v,want %v", service, kind, got, want)
				}
			}
		}
	}
	check(false)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled',locked_by=NULL,locked_until=NULL,finished_at=now()`); err != nil {
		t.Fatal(err)
	}
	check(true)
}
