package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

const backlogQuery = `
	SELECT
		count(*) FILTER (
			WHERE status IN ('queued', 'retry')
			  AND attempts < max_attempts
			  AND run_after <= statement_timestamp()
		),
		count(*) FILTER (
			WHERE status IN ('queued', 'retry')
			  AND attempts < max_attempts
			  AND run_after > statement_timestamp()
		),
		count(*) FILTER (
			WHERE status = 'processing'
			  AND locked_until < statement_timestamp()
		)
	FROM jobs`

type backlogQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type BacklogCollector struct {
	queryer backlogQueryer
	timeout time.Duration
	desc    *prometheus.Desc
}

func NewBacklogCollector(queryer backlogQueryer, timeout time.Duration) (*BacklogCollector, error) {
	if queryer == nil {
		return nil, errors.New("job backlog queryer is required")
	}
	if timeout <= 0 {
		return nil, errors.New("job backlog collection timeout must be positive")
	}
	return &BacklogCollector{
		queryer: queryer,
		timeout: timeout,
		desc: prometheus.NewDesc(
			"amocrm_jobs_backlog",
			"Exact current PostgreSQL job backlog by bounded eligibility kind.",
			[]string{"kind"}, nil,
		),
	}, nil
}

func (c *BacklogCollector) Describe(metrics chan<- *prometheus.Desc) {
	metrics <- c.desc
}

func (c *BacklogCollector) Collect(metrics chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	var ready, scheduled, expiredLease int64
	if err := c.queryer.QueryRow(ctx, backlogQuery).Scan(&ready, &scheduled, &expiredLease); err != nil {
		metrics <- prometheus.NewInvalidMetric(c.desc, err)
		return
	}
	for _, value := range []struct {
		kind  string
		count int64
	}{
		{kind: "ready", count: ready},
		{kind: "scheduled", count: scheduled},
		{kind: "expired_lease", count: expiredLease},
	} {
		metrics <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(value.count), value.kind)
	}
}
