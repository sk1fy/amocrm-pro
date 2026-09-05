package jobs

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

type Metrics struct {
	executionDuration *prometheus.HistogramVec
	workflowJobs      *prometheus.CounterVec
	workflowDuration  *prometheus.HistogramVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		executionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "amocrm", Subsystem: "jobs", Name: "execution_duration_seconds",
			Help: "Durably finalized job attempt execution time by bounded service and outcome.", Buckets: prometheus.DefBuckets,
		}, []string{"service", "outcome"}),
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
	registerer.MustRegister(metrics.workflowJobs, metrics.workflowDuration, metrics.executionDuration)
	return metrics
}

func (m *Metrics) observe(jobType string, status Status, duration time.Duration) {
	if m == nil {
		return
	}
	outcome := string(status)
	switch status {
	case StatusCompleted, StatusRetry, StatusFailed, StatusDead, StatusCancelled:
	default:
		outcome = "other"
	}
	m.executionDuration.WithLabelValues(metricService(jobType), outcome).Observe(duration.Seconds())
	if !strings.HasPrefix(jobType, "workflow.") {
		return
	}
	workflow := strings.TrimPrefix(jobType, "workflow.")
	if _, known := services.JobService(jobType); !known {
		workflow = "other"
	}
	m.workflowJobs.WithLabelValues(workflow, outcome).Inc()
	m.workflowDuration.WithLabelValues(workflow, outcome).Observe(duration.Seconds())
}
