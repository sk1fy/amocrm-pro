package amocrm

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

// Metrics observes the single client that owns all pilot Go API v4 budgets.
// Labels never contain installation/account IDs, paths or payloads.
type Metrics struct {
	wait     *prometheus.HistogramVec
	requests *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "amocrm_budget_wait_seconds", Help: "Time waiting for the shared integration and account outgoing budgets.", Buckets: []float64{.001, .01, .05, .1, .25, .5, 1, 2, 5, 10}}, []string{"outcome"}), requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "amocrm_requests_total", Help: "Go API v4 requests by bounded response category."}, []string{"outcome"})}
	reg.MustRegister(m.wait, m.requests)
	return m
}

// SetMetrics must be called once at composition time, before serving requests.
func (c *Client) SetMetrics(m *Metrics) { c.metrics = m }
func (c *Client) waitBudget(ctx context.Context, access AccessToken) error {
	start := time.Now()
	err := c.limiter.wait(ctx, access.IntegrationID, access.AccountID)
	if c.metrics != nil {
		outcome := "admitted"
		if err != nil {
			outcome = "canceled"
		}
		c.metrics.wait.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}
	return err
}
func (c *Client) observeResponse(status int, err error) {
	if c.metrics == nil {
		return
	}
	outcome := "other"
	switch {
	case err != nil:
		outcome = "transport_error"
	case status == 429:
		outcome = "429"
	case status == 401:
		outcome = "401"
	case status >= 200 && status < 300:
		outcome = "2xx"
	case status >= 400 && status < 500:
		outcome = "4xx"
	case status >= 500:
		outcome = "5xx"
	}
	c.metrics.requests.WithLabelValues(outcome).Inc()
}
