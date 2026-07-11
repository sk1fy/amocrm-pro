package webhook

import "github.com/prometheus/client_golang/prometheus"

const (
	ingressScopeGlobal       = "global"
	ingressScopeInstallation = "installation"
	ingressOutcomeAllowed    = "allowed"
	ingressOutcomeRejected   = "rejected"
)

type IngressMetrics struct {
	decisions *prometheus.CounterVec
	entries   prometheus.Gauge
	evictions prometheus.Counter
}

func NewIngressMetrics(registerer prometheus.Registerer) *IngressMetrics {
	metrics := &IngressMetrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "webhook", Name: "ingress_limiter_decisions_total",
			Help: "Process-local webhook ingress limiter decisions.",
		}, []string{"scope", "outcome"}),
		entries: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "amocrm", Subsystem: "webhook", Name: "ingress_limiter_entries",
			Help: "Current number of per-installation ingress limiter entries.",
		}),
		evictions: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "amocrm", Subsystem: "webhook", Name: "ingress_limiter_evictions_total",
			Help: "Per-installation ingress limiter entries evicted after inactivity.",
		}),
	}
	registerer.MustRegister(metrics.decisions, metrics.entries, metrics.evictions)
	return metrics
}

func (m *IngressMetrics) observeDecision(scope string, allowed bool) {
	if m == nil {
		return
	}
	outcome := ingressOutcomeRejected
	if allowed {
		outcome = ingressOutcomeAllowed
	}
	m.decisions.WithLabelValues(scope, outcome).Inc()
}

func (m *IngressMetrics) addEntry() {
	if m != nil {
		m.entries.Inc()
	}
}

func (m *IngressMetrics) evictEntries(count int) {
	if m == nil || count == 0 {
		return
	}
	m.entries.Sub(float64(count))
	m.evictions.Add(float64(count))
}
