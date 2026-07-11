package webhook

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestWorkflowRouteMetricsUseBoundedLabels(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	metrics.observeLeadStatusRoute("self_effect")
	metrics.observeLeadStatusRoute("self_effect")

	if got := testutil.ToFloat64(metrics.routes.WithLabelValues("lead_status_transition", "self_effect")); got != 2 {
		t.Fatalf("self-effect routes = %v", got)
	}
}
