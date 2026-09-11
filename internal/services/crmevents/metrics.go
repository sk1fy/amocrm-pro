package crmevents

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type Snapshot struct {
	States                                       map[string]int64
	EnrichmentStates                             map[string]int64
	CoverageStates                               map[string]int64
	AgeSeconds, LagSeconds, EnrichmentAgeSeconds float64
	CoverageGapSeconds                           float64
	CoverageGapValid                             bool
	Processed, Inserted, Updated, Deduplicated   int64
}

func metricState(state string) string {
	switch state {
	case "queued", "running", "retry", "paused", "completed", "failed":
		return state
	default:
		return "other"
	}
}

func enrichmentMetricState(state string) string {
	switch state {
	case serviceapi.EnrichmentPending, serviceapi.EnrichmentReady, serviceapi.EnrichmentUnavailable, serviceapi.EnrichmentRetry, serviceapi.EnrichmentError:
		return state
	default:
		return "other"
	}
}

func coverageMetricState(state string) string {
	switch state {
	case serviceapi.CoverageUnknown, serviceapi.CoveragePartial, serviceapi.CoverageVerified:
		return state
	default:
		return "other"
	}
}

// sourceCoverageClass is the source-level analogue of periodState for the
// retained window: unknown without a continuous range; partial when retained
// history starts before verified continuous_from; otherwise verified.
func sourceCoverageClass(continuousFrom, continuousTo, retainedFrom *time.Time) string {
	if continuousFrom == nil || continuousTo == nil {
		return serviceapi.CoverageUnknown
	}
	if retainedFrom != nil && retainedFrom.Before(*continuousFrom) {
		return serviceapi.CoveragePartial
	}
	return serviceapi.CoverageVerified
}

// Collector exports bounded service-level labels, never installation/user IDs.
func (s *Service) Collector() prometheus.Collector {
	return &collector{snapshot: s.repository.MetricsSnapshot}
}

type collector struct {
	snapshot func(context.Context) (Snapshot, error)
}

var (
	queueDesc         = prometheus.NewDesc("crm_events_jobs", "CRM Events owner jobs by state", []string{"state"}, nil)
	ageDesc           = prometheus.NewDesc("crm_events_oldest_job_age_seconds", "Oldest unfinished owner job", nil, nil)
	lagDesc           = prometheus.NewDesc("crm_events_max_lag_seconds", "Largest lag of an admitted source with verified progress", nil, nil)
	counterDesc       = prometheus.NewDesc("crm_events_events_total", "Persisted page event counters including replay passes", []string{"outcome"}, nil)
	enrichmentDesc    = prometheus.NewDesc("crm_events_enrichment", "CRM Events enrichment objects by state", []string{"state"}, nil)
	enrichmentAgeDesc = prometheus.NewDesc("crm_events_enrichment_oldest_age_seconds", "Oldest unfinished enrichment object", nil, nil)
	coverageDesc      = prometheus.NewDesc("crm_events_coverage", "Enabled CRM Events sources by retained-window coverage class", []string{"state"}, nil)
	coverageGapDesc   = prometheus.NewDesc("crm_events_coverage_gap_seconds", "Largest age since the newest coverage window of an enabled source", nil, nil)
	upDesc            = prometheus.NewDesc("crm_events_metrics_up", "Whether the owner metrics snapshot succeeded", nil, nil)
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{queueDesc, ageDesc, lagDesc, counterDesc, enrichmentDesc, enrichmentAgeDesc, coverageDesc, coverageGapDesc, upDesc} {
		ch <- d
	}
}
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snapshot, err := c.snapshot(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, 0)
		return
	}
	states := map[string]int64{}
	for state, n := range snapshot.States {
		states[metricState(state)] += n
	}
	for state, n := range states {
		ch <- prometheus.MustNewConstMetric(queueDesc, prometheus.GaugeValue, float64(n), state)
	}
	ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(ageDesc, prometheus.GaugeValue, max(0, snapshot.AgeSeconds))
	ch <- prometheus.MustNewConstMetric(lagDesc, prometheus.GaugeValue, max(0, snapshot.LagSeconds))
	for name, n := range map[string]int64{"processed": snapshot.Processed, "inserted": snapshot.Inserted, "updated": snapshot.Updated, "deduplicated": snapshot.Deduplicated} {
		ch <- prometheus.MustNewConstMetric(counterDesc, prometheus.CounterValue, float64(n), name)
	}
	enrichment := map[string]int64{serviceapi.EnrichmentPending: 0, serviceapi.EnrichmentReady: 0, serviceapi.EnrichmentUnavailable: 0, serviceapi.EnrichmentRetry: 0, serviceapi.EnrichmentError: 0, "other": 0}
	for state, n := range snapshot.EnrichmentStates {
		enrichment[enrichmentMetricState(state)] += n
	}
	for state, n := range enrichment {
		ch <- prometheus.MustNewConstMetric(enrichmentDesc, prometheus.GaugeValue, float64(n), state)
	}
	ch <- prometheus.MustNewConstMetric(enrichmentAgeDesc, prometheus.GaugeValue, max(0, snapshot.EnrichmentAgeSeconds))
	coverage := map[string]int64{serviceapi.CoverageUnknown: 0, serviceapi.CoveragePartial: 0, serviceapi.CoverageVerified: 0, "other": 0}
	for state, n := range snapshot.CoverageStates {
		coverage[coverageMetricState(state)] += n
	}
	for state, n := range coverage {
		ch <- prometheus.MustNewConstMetric(coverageDesc, prometheus.GaugeValue, float64(n), state)
	}
	if snapshot.CoverageGapValid {
		ch <- prometheus.MustNewConstMetric(coverageGapDesc, prometheus.GaugeValue, max(0, snapshot.CoverageGapSeconds))
	}
}
