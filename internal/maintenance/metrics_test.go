package maintenance

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCleanupMetricsObserveOutcomesAndBoundedPressure(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.observe(time.Now(), Result{
		LockAcquired: true, InboxEvents: 3, WebhookDeliveries: 2,
		InboxEventsLimitReached: true,
	}, nil)
	metrics.observe(time.Now(), Result{}, nil)
	metrics.observe(time.Now(), Result{}, errors.New("database unavailable"))

	if got := testutil.ToFloat64(metrics.passes.WithLabelValues("completed")); got != 1 {
		t.Fatalf("completed passes = %v", got)
	}
	if got := testutil.ToFloat64(metrics.passes.WithLabelValues("skipped")); got != 1 {
		t.Fatalf("skipped passes = %v", got)
	}
	if got := testutil.ToFloat64(metrics.passes.WithLabelValues("error")); got != 1 {
		t.Fatalf("error passes = %v", got)
	}
	if got := testutil.ToFloat64(metrics.deleted.WithLabelValues(recordInboxEvent)); got != 3 {
		t.Fatalf("deleted inbox events = %v", got)
	}
	if got := testutil.ToFloat64(metrics.deleted.WithLabelValues(recordWebhookDelivery)); got != 2 {
		t.Fatalf("deleted deliveries = %v", got)
	}
	if got := testutil.ToFloat64(metrics.batchLimit.WithLabelValues(recordInboxEvent)); got != 1 {
		t.Fatalf("inbox batch limit = %v", got)
	}
}
