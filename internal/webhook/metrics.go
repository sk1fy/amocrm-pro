package webhook

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	routes *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{routes: prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "amocrm", Subsystem: "workflow", Name: "routes_total",
		Help: "Durably committed workflow routing decisions.",
	}, []string{"workflow", "disposition"})}
	registerer.MustRegister(metrics.routes)
	return metrics
}

func (m *Metrics) observeLeadStatusRoute(disposition string) {
	if m == nil {
		return
	}
	m.routes.WithLabelValues("lead_status_transition", disposition).Inc()
}
