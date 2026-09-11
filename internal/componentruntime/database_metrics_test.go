package componentruntime

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestDatabaseSizeCollectorUnavailableDoesNotPublishZeroBytes(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(newDBSizeCollector(nil, "activity"))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "component_db_size_up" {
		t.Fatalf("families=%v", families)
	}
	metric := families[0].Metric[0]
	if metric.Gauge.GetValue() != 0 || len(metric.Label) != 1 || metric.Label[0].GetName() != "service" || metric.Label[0].GetValue() != "activity" {
		t.Fatalf("misleading size outage: %+v", metric)
	}
}

func TestDatabaseSizeCollectorReportsOwnerBytes(t *testing.T) {
	pool := testkit.Postgres(t)
	registry := prometheus.NewRegistry()
	RegisterPoolMetrics(registry, pool, "crm-events")
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	up := false
	var size float64
	for _, family := range families {
		switch family.GetName() {
		case "component_db_size_up":
			if family.Metric[0].Label[0].GetValue() != "crm-events" || family.Metric[0].Gauge.GetValue() != 1 {
				t.Fatalf("up=%+v", family.Metric[0])
			}
			up = true
		case "component_db_size_bytes":
			if family.Metric[0].Label[0].GetValue() != "crm-events" {
				t.Fatalf("tenant-like size label: %+v", family.Metric[0].Label)
			}
			size = family.Metric[0].Gauge.GetValue()
		}
	}
	if !up || size <= 0 {
		t.Fatalf("up=%t size=%v", up, size)
	}
}
