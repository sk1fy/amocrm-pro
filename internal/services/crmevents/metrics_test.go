package crmevents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestMetricStateMapsUnknownValuesToOther(t *testing.T) {
	if metricState("queued") != "queued" || metricState("broken") != "other" {
		t.Fatalf("job states queued=%s broken=%s", metricState("queued"), metricState("broken"))
	}
	if enrichmentMetricState(serviceapi.EnrichmentPending) != serviceapi.EnrichmentPending || enrichmentMetricState("mystery") != "other" {
		t.Fatal("enrichment states escaped the finite set")
	}
	if coverageMetricState(serviceapi.CoverageVerified) != serviceapi.CoverageVerified || coverageMetricState(serviceapi.CoverageStale) != "other" {
		t.Fatal("coverage states escaped the finite set")
	}
}

func TestMetricsCollectorUnavailableDoesNotPublishZeroBacklog(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(&collector{snapshot: func(context.Context) (Snapshot, error) {
		return Snapshot{}, errors.New("owner database unavailable")
	}})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "crm_events_metrics_up" || families[0].Metric[0].Gauge.GetValue() != 0 {
		t.Fatalf("misleading outage snapshot: %+v", families)
	}
}

func TestMetricsCollectorEmitsBoundedCoverageAndUnknownStates(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(&collector{snapshot: func(context.Context) (Snapshot, error) {
		return Snapshot{
			States:               map[string]int64{"queued": 2, "invented": 4},
			EnrichmentStates:     map[string]int64{serviceapi.EnrichmentPending: 3, "ghost": 1},
			CoverageStates:       map[string]int64{serviceapi.CoverageVerified: 1, serviceapi.CoverageStale: 2},
			AgeSeconds:           9,
			LagSeconds:           4,
			EnrichmentAgeSeconds: 8,
			CoverageGapSeconds:   5,
			CoverageGapValid:     true,
			Processed:            10,
			Inserted:             7,
			Updated:              2,
			Deduplicated:         1,
		}, nil
	}})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]float64{}
	gauges := map[string]float64{}
	counters := map[string]float64{}
	for _, family := range families {
		switch family.GetName() {
		case "crm_events_jobs", "crm_events_enrichment", "crm_events_coverage":
			series := map[string]float64{}
			for _, metric := range family.Metric {
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "state" {
					t.Fatalf("%s labels=%v", family.GetName(), metric.Label)
				}
				series[metric.Label[0].GetValue()] = metric.Gauge.GetValue()
			}
			got[family.GetName()] = series
		case "crm_events_events_total":
			for _, metric := range family.Metric {
				counters[metric.Label[0].GetValue()] = metric.Counter.GetValue()
			}
		default:
			if len(family.Metric) != 1 {
				t.Fatalf("%s series=%d", family.GetName(), len(family.Metric))
			}
			gauges[family.GetName()] = family.Metric[0].Gauge.GetValue()
		}
	}
	if got["crm_events_jobs"]["queued"] != 2 || got["crm_events_jobs"]["other"] != 4 {
		t.Fatalf("jobs=%v", got["crm_events_jobs"])
	}
	if got["crm_events_enrichment"][serviceapi.EnrichmentPending] != 3 || got["crm_events_enrichment"]["other"] != 1 {
		t.Fatalf("enrichment=%v", got["crm_events_enrichment"])
	}
	if got["crm_events_coverage"][serviceapi.CoverageVerified] != 1 || got["crm_events_coverage"]["other"] != 2 {
		t.Fatalf("coverage=%v", got["crm_events_coverage"])
	}
	for _, required := range []string{serviceapi.CoverageUnknown, serviceapi.CoveragePartial, serviceapi.CoverageVerified, "other"} {
		if _, ok := got["crm_events_coverage"][required]; !ok {
			t.Fatalf("missing coverage state %s", required)
		}
	}
	if gauges["crm_events_metrics_up"] != 1 || gauges["crm_events_oldest_job_age_seconds"] != 9 || gauges["crm_events_max_lag_seconds"] != 4 {
		t.Fatalf("gauges=%v", gauges)
	}
	if gauges["crm_events_coverage_gap_seconds"] != 5 || gauges["crm_events_enrichment_oldest_age_seconds"] != 8 {
		t.Fatalf("gap gauges=%v", gauges)
	}
	if counters["processed"] != 10 || counters["inserted"] != 7 || counters["updated"] != 2 || counters["deduplicated"] != 1 {
		t.Fatalf("counters=%v", counters)
	}
}

func TestSourceCoverageClassMatchesRetainedWindow(t *testing.T) {
	from := time.Unix(200, 0).UTC()
	to := time.Unix(300, 0).UTC()
	retainedInside := time.Unix(250, 0).UTC()
	retainedBefore := time.Unix(100, 0).UTC()
	if sourceCoverageClass(nil, &to, nil) != serviceapi.CoverageUnknown || sourceCoverageClass(&from, nil, nil) != serviceapi.CoverageUnknown {
		t.Fatal("missing continuous range must be unknown")
	}
	if sourceCoverageClass(&from, &to, nil) != serviceapi.CoverageVerified {
		t.Fatal("continuous range without retained frontier is verified")
	}
	if sourceCoverageClass(&from, &to, &retainedInside) != serviceapi.CoverageVerified {
		t.Fatal("retained inside continuous range is verified")
	}
	if sourceCoverageClass(&from, &to, &retainedBefore) != serviceapi.CoveragePartial {
		t.Fatal("default 7-day retain vs 2-day continuous must be partial, not verified")
	}
}

func TestMetricsCollectorOmitsCoverageGapWhenNoVerifiedProgress(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(&collector{snapshot: func(context.Context) (Snapshot, error) {
		return Snapshot{
			CoverageStates:     map[string]int64{serviceapi.CoverageUnknown: 1},
			CoverageGapSeconds: 0,
		}, nil
	}})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "crm_events_coverage_gap_seconds" {
			t.Fatal("unpublished verified progress must not look like a zero gap")
		}
	}
}
