package activitybridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"testing"
)

func TestUnavailableDeliveryMetricsDoNotPublishZeroBacklog(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister((&Bridge{}).Collector())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "activity_delivery_metrics_up" || families[0].Metric[0].Gauge.GetValue() != 0 {
		t.Fatalf("misleading outage snapshot: %+v", families)
	}
}
