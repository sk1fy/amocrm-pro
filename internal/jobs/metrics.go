package jobs

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	workflowJobs     *prometheus.CounterVec
	workflowDuration *prometheus.HistogramVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		workflowJobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "workflow", Name: "jobs_total",
			Help: "Durably finalized workflow job attempts by outcome.",
		}, []string{"workflow", "outcome"}),
		workflowDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "amocrm", Subsystem: "workflow", Name: "duration_seconds",
			Help:    "Workflow job attempt duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"workflow", "outcome"}),
	}
	registerer.MustRegister(metrics.workflowJobs, metrics.workflowDuration)
	return metrics
}

func (m *Metrics) observe(jobType string, status Status, duration time.Duration) {
	if m == nil || !strings.HasPrefix(jobType, "workflow.") {
		return
	}
	workflow := strings.TrimPrefix(jobType, "workflow.")
	outcome := string(status)
	m.workflowJobs.WithLabelValues(workflow, outcome).Inc()
	m.workflowDuration.WithLabelValues(workflow, outcome).Observe(duration.Seconds())
}
