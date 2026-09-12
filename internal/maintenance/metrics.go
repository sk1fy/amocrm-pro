package maintenance

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	recordWidgetToken       = "used_widget_token"
	recordIdempotencyKey    = "idempotency_key"
	recordOAuthState        = "oauth_state"
	recordInboxEvent        = "inbox_event"
	recordWebhookDelivery   = "webhook_delivery"
	recordOutboundEffect    = "outbound_effect"
	recordWorkflowRun       = "workflow_run"
	recordRuleConfiguration = "rule_configuration"
	recordJob               = "job"
	recordTombstone         = "webhook_event_tombstone"
	recordAudit             = "audit_log"
	recordCommandReceipt    = "activity_command_receipt"
)

type Metrics struct {
	lastSuccess prometheus.Gauge
	passes      *prometheus.CounterVec
	duration    prometheus.Histogram
	deleted     *prometheus.CounterVec
	batchLimit  *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "amocrm", Subsystem: "cleanup", Name: "last_success_timestamp_seconds",
			Help: "Unix time of the last committed cleanup pass; zero until first success.",
		}),
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
	registerer.MustRegister(metrics.passes, metrics.duration, metrics.deleted, metrics.batchLimit, metrics.lastSuccess)
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
	m.lastSuccess.SetToCurrentTime()
	m.passes.WithLabelValues("completed").Inc()
	observations := []struct {
		record       string
		deleted      int64
		limitReached bool
	}{
		{recordWidgetToken, result.WidgetTokens, result.WidgetTokensLimitReached},
		{recordIdempotencyKey, result.IdempotencyKeys, result.IdempotencyLimitReached},
		{recordOAuthState, result.OAuthStates, result.OAuthStatesLimitReached},
		{recordInboxEvent, result.InboxEvents, result.InboxEventsLimitReached},
		{recordWebhookDelivery, result.WebhookDeliveries, result.DeliveriesLimitReached},
		{recordOutboundEffect, result.OutboundEffects, result.OutboundEffectsLimitReached},
		{recordWorkflowRun, result.WorkflowRuns, result.WorkflowRunsLimitReached},
		{recordRuleConfiguration, result.RuleConfigurations, result.RuleConfigurationsLimitReached},
		{recordJob, result.Jobs, result.JobsLimitReached},
		{recordTombstone, result.Tombstones, result.TombstonesLimitReached},
		{recordAudit, result.Audit, result.AuditLimitReached},
		{recordCommandReceipt, result.CommandReceipts, result.CommandReceiptsLimitReached},
	}
	for _, observation := range observations {
		m.deleted.WithLabelValues(observation.record).Add(float64(observation.deleted))
		if observation.limitReached {
			m.batchLimit.WithLabelValues(observation.record).Inc()
		}
	}
}
