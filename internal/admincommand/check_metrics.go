package admincommand

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/connectioncheck"
	"time"
)

type CheckMetrics struct {
	Total      *prometheus.CounterVec
	Duration   prometheus.Histogram
	Inflight   prometheus.Gauge
	Age        prometheus.Gauge
	Stale      prometheus.Counter
	Lag        prometheus.Gauge
	Backlog    prometheus.Gauge
	Population *prometheus.GaugeVec
	Enabled    prometheus.Gauge
}

func NewCheckMetrics(r prometheus.Registerer) *CheckMetrics {
	m := &CheckMetrics{
		Backlog:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "admin_connection_check_backlog", Help: "Due active verification reservations in the database."}),
		Total:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "admin_connection_checks_total", Help: "Completed check attempts by bounded result."}, []string{"classification"}),
		Duration:   prometheus.NewHistogram(prometheus.HistogramOpts{Name: "admin_connection_check_duration_seconds", Help: "Live check duration.", Buckets: prometheus.DefBuckets}),
		Inflight:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "admin_connection_checks_inflight", Help: "Live checks in this worker."}),
		Age:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "admin_connection_verification_age_seconds", Help: "Oldest known verification age among active installations."}),
		Stale:      prometheus.NewCounter(prometheus.CounterOpts{Name: "admin_connection_verification_stale_total", Help: "Overdue checks admitted by this scheduler."}),
		Lag:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "admin_connection_check_scheduler_lag_seconds", Help: "Age of oldest due active check reservation."}),
		Population: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "admin_connection_verification_installations", Help: "Active installation population by verification state (database-wide snapshot)."}, []string{"state"}),
		Enabled:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "admin_connection_check_scheduler_enabled", Help: "Whether this worker schedules checks."}),
	}
	r.MustRegister(m.Total, m.Duration, m.Inflight, m.Age, m.Stale, m.Lag, m.Population, m.Enabled, m.Backlog)
	return m
}
func (m *CheckMetrics) Refresh(ctx context.Context, conn *pgx.Conn, now time.Time) error {
	var unknown, stale, fresh, failed, backlog int64
	var age, lag float64
	err := conn.QueryRow(ctx, `SELECT
 count(*) FILTER(WHERE ck.observed_at IS NULL),
 count(*) FILTER(WHERE ck.observed_at<$1::timestamptz-make_interval(secs=>$2)),
 count(*) FILTER(WHERE ck.classification='verified_ok' AND ck.observed_at>=$1::timestamptz-make_interval(secs=>$2)),
 count(*) FILTER(WHERE ck.classification<>'verified_ok' AND ck.observed_at>=$1::timestamptz-make_interval(secs=>$2)),
 COALESCE(max(greatest(0,extract(epoch FROM ($1::timestamptz-ck.observed_at)))),0),
 COALESCE(max(greatest(0,extract(epoch FROM ($1::timestamptz-greatest(sch.next_check_at,ck.next_check_at))))),0)
  ,count(*) FILTER(WHERE sch.next_check_at<=$1 AND (ck.next_check_at IS NULL OR ck.next_check_at<=$1))
 FROM installations i LEFT JOIN oauth_credentials oc ON oc.installation_id=i.id
 LEFT JOIN installation_checks ck ON ck.installation_id=i.id AND ck.credential_version=COALESCE(oc.token_version,0) AND ck.installation_status=i.status
 LEFT JOIN installation_check_schedule sch ON sch.installation_id=i.id WHERE i.status='active'`, now, connectioncheck.FreshFor.Seconds()).Scan(&unknown, &stale, &fresh, &failed, &age, &lag, &backlog)
	if err != nil {
		return err
	}
	m.Population.WithLabelValues("unknown").Set(float64(unknown))
	m.Population.WithLabelValues("stale").Set(float64(stale))
	m.Population.WithLabelValues("fresh").Set(float64(fresh))
	m.Population.WithLabelValues("failed").Set(float64(failed))
	m.Backlog.Set(float64(backlog))
	m.Age.Set(age)
	m.Lag.Set(lag)
	return nil
}
