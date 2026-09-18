package amocrm

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics observes the single client that owns all pilot Go API v4 budgets.
// Labels never contain installation/account IDs, paths or payloads: a
// saturated account is diagnosed from structured logs by request id.
type Metrics struct {
	wait      *prometheus.HistogramVec
	requests  *prometheus.CounterVec
	waiting   prometheus.Gauge
	rejected  *prometheus.CounterVec
	throttled *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		wait:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "amocrm_budget_wait_seconds", Help: "Time waiting for the pair and account outgoing budgets.", Buckets: []float64{.001, .01, .05, .1, .25, .5, 1, 2, 5, 10}}, []string{"outcome"}),
		requests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "amocrm_requests_total", Help: "Go API v4 requests by bounded response category."}, []string{"outcome"}),
		waiting:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "amocrm_budget_waiting", Help: "Calls currently waiting for an outgoing budget slot."}),
		rejected:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "amocrm_budget_rejected_total", Help: "Calls refused because the bounded wait queue of that scope is full."}, []string{"scope"}),
		throttled: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "amocrm_budget_throttled_total", Help: "Admissions delayed by the pair or the account budget."}, []string{"scope"}),
	}
	reg.MustRegister(m.wait, m.requests, m.waiting, m.rejected, m.throttled)
	return m
}

// SetMetrics must be called once at composition time, before serving requests.
func (c *Client) SetMetrics(m *Metrics) {
	c.metrics = m
	c.limiter.setMetrics(m)
}

func (c *Client) waitBudget(ctx context.Context, access AccessToken) error {
	return c.waitKey(ctx, budgetKey{AccountID: access.AccountID, IntegrationID: access.IntegrationID})
}

func (c *Client) waitKey(ctx context.Context, key budgetKey) error {
	start := time.Now()
	err := c.limiter.wait(ctx, key)
	c.observeWait(start, err)
	return err
}

func (c *Client) observeWait(start time.Time, err error) {
	if c.metrics == nil {
		return
	}
	c.metrics.wait.WithLabelValues(waitOutcome(err)).Observe(time.Since(start).Seconds())
}

func waitOutcome(err error) string {
	var api *APIError
	switch {
	case err == nil:
		return "admitted"
	case errors.As(err, &api) && api.Kind == ErrorOverloaded:
		return "overloaded"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "canceled"
	}
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
