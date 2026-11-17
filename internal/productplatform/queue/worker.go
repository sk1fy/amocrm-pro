package queue

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Handler func(context.Context, Job) error

type Config struct {
	Workers        int
	PollInterval   time.Duration
	SweepInterval  time.Duration
	QueryTimeout   time.Duration
	HandlerTimeout time.Duration
	LeaseDuration  time.Duration
}

func DefaultConfig(workers int) Config {
	return Config{
		Workers: workers, PollInterval: 250 * time.Millisecond,
		SweepInterval: 5 * time.Second, QueryTimeout: 2 * time.Second,
		HandlerTimeout: 30 * time.Second, LeaseDuration: 45 * time.Second,
	}
}

type Worker struct {
	store    Store
	handlers map[Kind]Handler
	kinds    []Kind
	config   Config
	metrics  queueMetrics
}

func NewWorker(store Store, handlers map[Kind]Handler, cfg Config, reg prometheus.Registerer) Worker {
	var kinds []Kind
	for kind := range handlers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return Worker{store: store, handlers: maps.Clone(handlers), kinds: kinds, config: cfg, metrics: newMetrics(reg)}
}

func (w Worker) Run(ctx context.Context) {
	var group sync.WaitGroup
	group.Go(func() { w.maintain(ctx) })
	if len(w.kinds) > 0 {
		for range w.config.Workers {
			group.Go(func() { w.work(ctx) })
		}
	}
	group.Wait()
}

func (w Worker) work(ctx context.Context) {
	for ctx.Err() == nil {
		if w.doJob(ctx) {
			continue
		}
		timer := time.NewTimer(w.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w Worker) doJob(ctx context.Context) bool {
	queryCtx, cancel := context.WithTimeout(ctx, w.config.QueryTimeout)
	lease, found, err := w.store.Claim(queryCtx, w.kinds, w.config.LeaseDuration)
	cancel()
	if err != nil {
		w.recordDBError(ctx, "claim")
		return false
	}
	if !found || ctx.Err() != nil {
		return false
	}

	handlerCtx, cancel := context.WithTimeout(ctx, w.config.HandlerTimeout)
	handlerErr := w.handlers[lease.Kind](handlerCtx, lease.Job)
	if handlerErr == nil {
		handlerErr = handlerCtx.Err()
	}
	cancel()
	if ctx.Err() != nil {
		return false
	}

	queryCtx, cancel = context.WithTimeout(ctx, w.config.QueryTimeout)
	defer cancel()
	status, err := w.store.Finish(queryCtx, lease, handlerErr)
	switch {
	case errors.Is(err, ErrLeaseLost):
		w.metrics.outcomes.WithLabelValues("lease_lost").Inc()
	case err != nil:
		w.recordDBError(ctx, "finish")
	default:
		w.metrics.outcomes.WithLabelValues(string(status)).Inc()
	}
	return true
}

func (w Worker) maintain(ctx context.Context) {
	ticker := time.NewTicker(w.config.SweepInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		w.recover(ctx)
		w.observe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w Worker) recover(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, w.config.QueryTimeout)
	defer cancel()
	retried, dead, err := w.store.Recover(queryCtx)
	if err != nil {
		w.recordDBError(ctx, "recover")
		return
	}
	w.metrics.outcomes.WithLabelValues("recovered_queued").Add(float64(retried))
	w.metrics.outcomes.WithLabelValues("recovered_dead").Add(float64(dead))
}

func (w Worker) observe(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, w.config.QueryTimeout)
	defer cancel()
	state, err := w.store.stats(queryCtx)
	if err != nil {
		w.metrics.snapshotOK.Set(0)
		w.recordDBError(ctx, "observe")
		return
	}
	w.metrics.depth.WithLabelValues(string(Queued)).Set(float64(state.queued))
	w.metrics.depth.WithLabelValues(string(Running)).Set(float64(state.running))
	w.metrics.expired.Set(float64(state.expired))
	w.metrics.oldest.Set(state.oldestSeconds)
	w.metrics.snapshotOK.Set(1)
}

func (w Worker) recordDBError(ctx context.Context, operation string) {
	if ctx.Err() == nil {
		w.metrics.dbErrors.WithLabelValues(operation).Inc()
	}
}

type queueMetrics struct {
	depth      *prometheus.GaugeVec
	expired    prometheus.Gauge
	oldest     prometheus.Gauge
	snapshotOK prometheus.Gauge
	outcomes   *prometheus.CounterVec
	dbErrors   *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) queueMetrics {
	m := queueMetrics{
		depth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "platform_queue_depth", Help: "Jobs in active queue states in the latest database sample.",
		}, []string{"status"}),
		expired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "platform_queue_expired_leases", Help: "Expired running job leases in the latest database sample.",
		}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "platform_queue_oldest_ready_seconds", Help: "Time since the oldest ready job became available, in the latest database sample.",
		}),
		snapshotOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "platform_queue_snapshot_ok", Help: "Whether the latest database sample succeeded.",
		}),
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "platform_queue_outcomes_total", Help: "Job transitions or lost leases observed in this process.",
		}, []string{"outcome"}),
		dbErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "platform_queue_database_errors_total", Help: "Queue database operation failures in this process.",
		}, []string{"operation"}),
	}
	for _, status := range []Status{Queued, Running} {
		m.depth.WithLabelValues(string(status)).Set(0)
	}
	for _, outcome := range []string{"queued", "succeeded", "dead", "lease_lost", "recovered_queued", "recovered_dead"} {
		m.outcomes.WithLabelValues(outcome).Add(0)
	}
	for _, operation := range []string{"claim", "finish", "recover", "observe"} {
		m.dbErrors.WithLabelValues(operation).Add(0)
	}
	reg.MustRegister(m.depth, m.expired, m.oldest, m.snapshotOK, m.outcomes, m.dbErrors)
	return m
}
