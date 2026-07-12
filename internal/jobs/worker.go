package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

type WorkerConfig struct {
	ID            string
	PollInterval  time.Duration
	LeaseDuration time.Duration
	JobTimeout    time.Duration
	BatchSize     int
	ReapBatchSize int
	Concurrency   int
	DrainTimeout  time.Duration
	ClaimTimeout  time.Duration
}

type Worker struct {
	store            *Store
	logger           *slog.Logger
	config           WorkerConfig
	handlers         map[string]Handler
	failureObservers map[string]FailureObserver
	metrics          *Metrics
}

func (w *Worker) SetMetrics(metrics *Metrics) {
	w.metrics = metrics
}

func NewWorker(
	store *Store,
	logger *slog.Logger,
	config WorkerConfig,
	handlers map[string]Handler,
	failureObserverSets ...map[string]FailureObserver,
) *Worker {
	registered := make(map[string]Handler, len(handlers))
	for jobType, handler := range handlers {
		registered[jobType] = handler
	}
	observers := make(map[string]FailureObserver)
	if len(failureObserverSets) > 0 {
		for jobType, observer := range failureObserverSets[0] {
			observers[jobType] = observer
		}
	}
	return &Worker{
		store: store, logger: logger, config: config,
		handlers: registered, failureObservers: observers,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()

	semaphore := make(chan struct{}, w.config.Concurrency)
	var active sync.WaitGroup

	w.poll(ctx, semaphore, &active)
	for {
		select {
		case <-ctx.Done():
			w.drain(&active)
			return nil
		case <-ticker.C:
			w.poll(ctx, semaphore, &active)
		}
	}
}

func (w *Worker) poll(ctx context.Context, semaphore chan struct{}, active *sync.WaitGroup) {
	available := cap(semaphore) - len(semaphore)
	if available <= 0 {
		return
	}
	limit := min(w.config.BatchSize, available)
	claimTimeout := w.config.ClaimTimeout
	if claimTimeout <= 0 {
		claimTimeout = 2 * time.Second
	}
	claimContext, cancel := context.WithTimeout(ctx, claimTimeout)
	claimed, err := w.store.ClaimWithObserver(
		claimContext, w.config.ID, limit, w.config.ReapBatchSize,
		w.config.LeaseDuration, w.dispatchFailure,
	)
	cancel()
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			w.logger.Error("claim jobs", "error", err)
		} else if errors.Is(err, context.DeadlineExceeded) {
			w.logger.Warn("claim jobs timed out", "timeout", claimTimeout)
		}
		return
	}

	for _, job := range claimed {
		semaphore <- struct{}{}
		active.Add(1)
		go func(job Job) {
			defer func() {
				<-semaphore
				active.Done()
			}()
			w.execute(ctx, job)
		}(job)
	}
}

func (w *Worker) execute(parent context.Context, job Job) {
	started := time.Now()
	logger := w.logger.With(
		"job_id", job.ID.String(),
		"job_type", job.Type,
		"attempt", job.Attempts,
	)
	if job.InstallationID != nil {
		logger = logger.With("installation_id", job.InstallationID.String())
	}

	ctx, cancel := context.WithTimeout(parent, w.config.JobTimeout)
	defer cancel()

	leaseDone := make(chan struct{})
	go w.heartbeat(ctx, cancel, job, leaseDone, logger)

	handler, ok := w.handlers[job.Type]
	var result json.RawMessage
	var err error
	if !ok {
		err = Permanent("unknown_job_type", errors.New("no handler registered for job type"))
	} else {
		result, err = w.invokeHandler(ctx, logger, handler, job)
	}
	cancel()
	<-leaseDone

	duration := time.Since(started)
	finalizeTimeout := min(5*time.Second, w.config.DrainTimeout)
	if finalizeTimeout <= 0 {
		finalizeTimeout = 5 * time.Second
	}
	finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer finalizeCancel()
	if err == nil {
		if completeErr := w.store.Complete(finalizeContext, job, w.config.ID, result, duration); completeErr != nil {
			logger.Error("complete job", "error", completeErr, "duration", duration)
			return
		}
		w.metrics.observe(job.Type, StatusCompleted, duration)
		logger.Info("job completed", "duration", duration)
		return
	}

	failure := Classify(err, job.Attempts)
	status, failErr := w.store.FailWithObserver(
		finalizeContext, job, w.config.ID, failure, duration, w.failureObservers[job.Type],
	)
	if failErr != nil {
		logger.Error("record job failure", "error", failErr, "job_error", err, "duration", duration)
		return
	}
	w.metrics.observe(job.Type, status, duration)
	logger.Warn("job failed", "error", err, "error_code", failure.Code, "status", status, "duration", duration)
}

func (w *Worker) dispatchFailure(
	ctx context.Context,
	tx TxExecutor,
	job Job,
	failure Failure,
	status Status,
) error {
	observer, exists := w.failureObservers[job.Type]
	if !exists {
		return nil
	}
	return observer(ctx, tx, job, failure, status)
}

func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, job Job, done chan<- struct{}, logger *slog.Logger) {
	defer close(done)
	interval := w.config.LeaseDuration / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			leaseCtx, leaseCancel := context.WithTimeout(context.Background(), min(interval, 2*time.Second))
			err := w.store.ExtendLease(leaseCtx, job.ID, w.config.ID, job.Attempts, w.config.LeaseDuration)
			leaseCancel()
			if err != nil {
				logger.Error("extend job lease", "error", err)
				cancel()
				return
			}
		}
	}
}

func (w *Worker) invokeHandler(
	ctx context.Context,
	logger *slog.Logger,
	handler Handler,
	job Job,
) (result json.RawMessage, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("job handler panic", "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			result = nil
			err = Retryable("handler_panic", 0, errors.New("job handler panicked"))
		}
	}()
	return handler(ctx, job)
}

func (w *Worker) drain(active *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		active.Wait()
		close(done)
	}()
	timeout := w.config.DrainTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	select {
	case <-done:
	case <-time.After(timeout):
		w.logger.Warn("worker drain deadline exceeded", "timeout", timeout)
	}
}
