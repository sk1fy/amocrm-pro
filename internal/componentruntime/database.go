package componentruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// openOwned rejects migration/superuser credentials, even in embedded mode.
func openOwned(ctx context.Context, dsn, service string, max int32, reg prometheus.Registerer) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid %s DSN", service)
	}
	cfg.MaxConns = max
	cfg.ConnConfig.RuntimeParams["application_name"] = service
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "2000"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "5000"
	if reg != nil {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "component_sql_duration_seconds", Help: "SQL round trip duration including server execution.", ConstLabels: prometheus.Labels{"service": service}, Buckets: prometheus.DefBuckets}, []string{"operation"})
		reg.MustRegister(h)
		cfg.ConnConfig.Tracer = sqlTracer{h: h}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open %s DB: %w", service, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var privileged bool
	err = pool.QueryRow(checkCtx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR has_schema_privilege(current_user,'public','CREATE') FROM pg_roles WHERE rolname=current_user`).Scan(&privileged)
	if err != nil || privileged {
		pool.Close()
		if err != nil {
			return nil, fmt.Errorf("verify %s runtime role: %w", service, err)
		}
		return nil, fmt.Errorf("%s runtime role must not have migration or administrative privileges", service)
	}
	RegisterPoolMetrics(reg, pool, service)
	return pool, nil
}

// RegisterPoolMetrics also observes existing Core pools without creating a new
// client or changing their ownership. Each process uses its own registry.
func RegisterPoolMetrics(reg prometheus.Registerer, pool *pgxpool.Pool, service string) {
	if reg != nil {
		for _, metric := range []struct {
			name, help string
			value      func(*pgxpool.Stat) float64
		}{
			{"connections_acquired", "Connections currently acquired.", func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }},
			{"connections_max", "Configured maximum connections.", func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }},
			{"acquire_seconds_total", "Cumulative connection acquire duration.", func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() }},
			{"empty_acquire_total", "Acquires that encountered an empty pool.", func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) }},
			{"canceled_acquire_total", "Connection acquisitions canceled by context.", func(s *pgxpool.Stat) float64 { return float64(s.CanceledAcquireCount()) }},
		} {
			m := metric
			reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "component_db_" + m.name, Help: m.help, ConstLabels: prometheus.Labels{"service": service}}, func() float64 { return m.value(pool.Stat()) }))
		}
	}
}

type queryStart struct{}
type sqlTracer struct{ h *prometheus.HistogramVec }
type queryTrace struct {
	start     time.Time
	operation string
}

func (s sqlTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	op := "other"
	words := strings.Fields(d.SQL)
	if len(words) > 0 {
		switch strings.ToLower(words[0]) {
		case "select", "insert", "update", "delete", "begin", "commit", "rollback":
			op = strings.ToLower(words[0])
		}
	}
	return context.WithValue(ctx, queryStart{}, queryTrace{time.Now(), op})
}
func (s sqlTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if t, ok := ctx.Value(queryStart{}).(queryTrace); ok {
		s.h.WithLabelValues(t.operation).Observe(time.Since(t.start).Seconds())
	}
}
