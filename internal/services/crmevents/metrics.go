package crmevents

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Snapshot struct {
	States                                     map[string]int64
	AgeSeconds, LagSeconds                     float64
	Processed, Inserted, Updated, Deduplicated int64
}

func metricState(state string) string {
	switch state {
	case "queued", "running", "retry", "paused", "completed", "failed":
		return state
	default:
		return "other"
	}
}

// Collector exports bounded service-level labels, never installation/user IDs.
func (s *Service) Collector() prometheus.Collector { return &collector{s: s} }

type collector struct{ s *Service }

var (
	queueDesc   = prometheus.NewDesc("crm_events_jobs", "CRM Events owner jobs by state", []string{"state"}, nil)
	ageDesc     = prometheus.NewDesc("crm_events_oldest_job_age_seconds", "Oldest unfinished owner job", nil, nil)
	lagDesc     = prometheus.NewDesc("crm_events_max_lag_seconds", "Largest lag of an admitted source with verified progress", nil, nil)
	counterDesc = prometheus.NewDesc("crm_events_events_total", "Persisted page event counters including replay passes", []string{"outcome"}, nil)
	upDesc      = prometheus.NewDesc("crm_events_metrics_up", "Whether the owner metrics snapshot succeeded", nil, nil)
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{queueDesc, ageDesc, lagDesc, counterDesc, upDesc} {
		ch <- d
	}
}
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snapshot, err := c.s.repository.MetricsSnapshot(ctx)
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
}
