package maintenance

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	recordWidgetToken     = "used_widget_token"
	recordIdempotencyKey  = "idempotency_key"
	recordInboxEvent      = "inbox_event"
	recordWebhookDelivery = "webhook_delivery"
)

type Metrics struct {
	passes     *prometheus.CounterVec
	duration   prometheus.Histogram
	deleted    *prometheus.CounterVec
	batchLimit *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		passes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "cleanup", Name: "passes_total",
			Help: "Cleanup passes by durable outcome.",
		}, []string{"outcome"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "amocrm", Subsystem: "cleanup", Name: "duration_seconds",
			Help:    "Cleanup pass duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		deleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "cleanup", Name: "rows_deleted_total",
			Help: "Rows deleted by bounded record kind.",
		}, []string{"record"}),
		batchLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "cleanup", Name: "batch_limit_total",
			Help: "Cleanup passes that exhausted the configured batch limit.",
		}, []string{"record"}),
	}
	registerer.MustRegister(metrics.passes, metrics.duration, metrics.deleted, metrics.batchLimit)
	return metrics
}

func (m *Metrics) observe(started time.Time, result Result, err error) {
	if m == nil {
		return
	}
	m.duration.Observe(time.Since(started).Seconds())
	if err != nil {
		m.passes.WithLabelValues("error").Inc()
		return
	}
	if !result.LockAcquired {
		m.passes.WithLabelValues("skipped").Inc()
		return
	}
	m.passes.WithLabelValues("completed").Inc()
	observations := []struct {
		record       string
		deleted      int64
		limitReached bool
	}{
		{recordWidgetToken, result.WidgetTokens, result.WidgetTokensLimitReached},
		{recordIdempotencyKey, result.IdempotencyKeys, result.IdempotencyLimitReached},
		{recordInboxEvent, result.InboxEvents, result.InboxEventsLimitReached},
		{recordWebhookDelivery, result.WebhookDeliveries, result.DeliveriesLimitReached},
	}
	for _, observation := range observations {
		m.deleted.WithLabelValues(observation.record).Add(float64(observation.deleted))
		if observation.limitReached {
			m.batchLimit.WithLabelValues(observation.record).Inc()
		}
	}
}
