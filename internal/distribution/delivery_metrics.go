package distribution

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type DeliveryCollector struct {
	pool                                                     *pgxpool.Pool
	count, age, gap, available, operations, receipts, errors *prometheus.Desc
}

func NewDeliveryCollector(pool *pgxpool.Pool) *DeliveryCollector {
	return &DeliveryCollector{pool: pool, count: prometheus.NewDesc("amocrm_distribution_delivery_messages", "Durable outbox messages by bounded kind/state", []string{"kind", "state"}, nil), age: prometheus.NewDesc("amocrm_distribution_delivery_oldest_seconds", "Age of oldest unacknowledged message", []string{"kind"}, nil), gap: prometheus.NewDesc("amocrm_distribution_recovery_gaps", "Scans carrying an irrecoverable historical transition gap", []string{"state"}, nil), available: prometheus.NewDesc("amocrm_distribution_delivery_collection_available", "Whether the bounded durable delivery query succeeded", nil, nil), operations: prometheus.NewDesc("amocrm_distribution_unfinished_operations", "Unfinished durable operations by bounded state", []string{"state"}, nil), errors: prometheus.NewDesc("amocrm_distribution_operation_errors", "Durable operations with a safe bounded error classification", []string{"class"}, nil), receipts: prometheus.NewDesc("amocrm_distribution_consumer_receipts", "Durable consumer dispositions including blocked normalization", []string{"consumer", "state"}, nil)}

}
func (c *DeliveryCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.count, c.age, c.gap, c.available, c.operations, c.receipts, c.errors} {
		ch <- d
	}
}
func (c *DeliveryCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, e := c.pool.Query(ctx, `SELECT 'events',state,count(*),COALESCE(extract(epoch FROM clock_timestamp()-min(created_at) FILTER(WHERE state<>'acknowledged')),0) FROM distribution_event_outbox GROUP BY state UNION ALL SELECT 'results',state,count(*),COALESCE(extract(epoch FROM clock_timestamp()-min(created_at) FILTER(WHERE state<>'acknowledged')),0) FROM distribution_result_outbox GROUP BY state`)
	if e != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	defer rows.Close()
	ages := map[string]float64{"events": 0, "results": 0}
	for rows.Next() {
		var kind, state string
		var count, age float64
		if e = rows.Scan(&kind, &state, &count, &age); e != nil {
			ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, count, kind, state)
		ages[kind] = max(ages[kind], age)
	}
	if rows.Err() != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	for kind, age := range ages {
		ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, age, kind)
	}
	for _, state := range []string{"pending", "scanning", "completed", "blocked"} {
		var n float64
		if e = c.pool.QueryRow(ctx, `SELECT count(*) FROM distribution_recovery_scans WHERE state=$1 AND gap_reason<>''`, state).Scan(&n); e != nil {
			ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.gap, prometheus.GaugeValue, n, state)
	}

	opRows, err := c.pool.Query(ctx, `SELECT state,count(*) FROM distribution_operations WHERE finished_at IS NULL GROUP BY state`)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	for opRows.Next() {
		var state string
		var n float64
		if err = opRows.Scan(&state, &n); err != nil {
			opRows.Close()
			ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.operations, prometheus.GaugeValue, n, state)
	}
	err = opRows.Err()
	opRows.Close()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	receiptRows, err := c.pool.Query(ctx, `SELECT consumer_id,state,count(*) FROM webhook_consumer_receipts GROUP BY consumer_id,state`)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	for receiptRows.Next() {
		var consumer, state string
		var n float64
		if err = receiptRows.Scan(&consumer, &state, &n); err != nil {
			receiptRows.Close()
			ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.receipts, prometheus.GaugeValue, n, consumer, state)
	}
	err = receiptRows.Err()
	receiptRows.Close()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	errorRows, err := c.pool.Query(ctx, `SELECT CASE
 WHEN error_code IN('reauth_required','permission_denied','capability_revoked','mapping_required') THEN 'authorization'
 WHEN error_code IN('source_unavailable','policy_unavailable','rate_limited') THEN 'availability'
 WHEN error_code='external_outcome_unproven' THEN 'unknown_effect'
 WHEN error_code IN('source_changed','manual_change_after_response','decision_expired') THEN 'precondition'
 ELSE 'other' END,count(*) FROM distribution_operations WHERE error_code IS NOT NULL GROUP BY 1`)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	for errorRows.Next() {
		var class string
		var n float64
		if err = errorRows.Scan(&class, &n); err != nil {
			break
		}
		ch <- prometheus.MustNewConstMetric(c.errors, prometheus.GaugeValue, n, class)
	}
	if err == nil {
		err = errorRows.Err()
	}
	errorRows.Close()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 1)
}
