package jobs

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestWorkflowMetricsIgnoreNonWorkflowJobs(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	metrics.observe("workflow.lead.status_transition", StatusCompleted, time.Second)
	metrics.observe("webhook.parse", StatusCompleted, time.Second)

	if got := testutil.ToFloat64(metrics.workflowJobs.WithLabelValues("lead.status_transition", "completed")); got != 1 {
		t.Fatalf("workflow completed jobs = %v", got)
	}
	if got := testutil.ToFloat64(metrics.workflowJobs.WithLabelValues("webhook.parse", "completed")); got != 0 {
		t.Fatalf("non-workflow jobs = %v", got)
	}
}
