package activitybridge

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

var (
	deliveryUp     = prometheus.NewDesc("activity_delivery_metrics_up", "Whether the last bounded Core outbox snapshot succeeded.", nil, nil)
	deliveryCount  = prometheus.NewDesc("activity_delivery_commands", "Core commands by finite delivery state.", []string{"state"}, nil)
	deliveryAge    = prometheus.NewDesc("activity_delivery_oldest_pending_seconds", "Age since Core acceptance of the oldest command awaiting delivery.", nil, nil)
	deliveryErrors = prometheus.NewDesc("activity_delivery_errors", "Core commands carrying a last delivery error by finite category.", []string{"code"}, nil)
)

type deliveryCollector struct{ pool *pgxpool.Pool }

// Collector reports only Core-owned delivery state. An unreachable Core DB
// yields up=0 without publishing misleading zero backlog. No tenant labels or
// payload/actor metadata are emitted, and gathering has a two-second deadline.
func (b *Bridge) Collector() prometheus.Collector { return &deliveryCollector{pool: b.pool} }
func (c *deliveryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- deliveryUp
	ch <- deliveryCount
	ch <- deliveryAge
	ch <- deliveryErrors
}
func (c *deliveryCollector) Collect(ch chan<- prometheus.Metric) {
	if c.pool == nil {
		ch <- prometheus.MustNewConstMetric(deliveryUp, prometheus.GaugeValue, 0)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.pool.Query(ctx, `SELECT outbox.status,
 CASE WHEN error_code IS NULL THEN '' WHEN error_code IN ('invalid_argument','unauthenticated','permission_denied','not_found','conflict','unavailable','deadline_exceeded','resource_exhausted','reauth_required','internal','delivery_attempts_exhausted') THEN error_code ELSE 'other' END AS category,
 count(*),COALESCE(max(EXTRACT(EPOCH FROM now()-receipt.created_at)) FILTER(WHERE outbox.status IN ('pending_delivery','delivering')),0)
 FROM activity_command_outbox outbox JOIN activity_command_receipts receipt USING(command_id)
 GROUP BY outbox.status,category`)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(deliveryUp, prometheus.GaugeValue, 0)
		return
	}
	defer rows.Close()
	counts := map[string]float64{"pending_delivery": 0, "delivering": 0, "accepted": 0, "failed": 0}
	errors := map[string]float64{}
	oldest := float64(0)
	for rows.Next() {
		var state, code string
		var count, age float64
		if err := rows.Scan(&state, &code, &count, &age); err != nil {
			ch <- prometheus.MustNewConstMetric(deliveryUp, prometheus.GaugeValue, 0)
			return
		}
		counts[state] += count
		if code != "" {
			errors[code] += count
		}
		oldest = max(oldest, age)
	}
	if rows.Err() != nil {
		ch <- prometheus.MustNewConstMetric(deliveryUp, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(deliveryUp, prometheus.GaugeValue, 1)
	for state, count := range counts {
		ch <- prometheus.MustNewConstMetric(deliveryCount, prometheus.GaugeValue, count, state)
	}
	for code, count := range errors {
		ch <- prometheus.MustNewConstMetric(deliveryErrors, prometheus.GaugeValue, count, code)
	}
	ch <- prometheus.MustNewConstMetric(deliveryAge, prometheus.GaugeValue, max(0, oldest))
}
