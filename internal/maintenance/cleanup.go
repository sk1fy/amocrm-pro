package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const cleanupAdvisoryLockID int64 = 6_584_483_612_447_211_903

type Policy struct {
	SafetyMargin             time.Duration
	WebhookInboxRetention    time.Duration
	WebhookDeliveryRetention time.Duration
	BatchSize                int
	MaxBatches               int
}

type Result struct {
	LockAcquired             bool
	WidgetTokens             int64
	IdempotencyKeys          int64
	InboxEvents              int64
	WebhookDeliveries        int64
	WidgetTokensLimitReached bool
	IdempotencyLimitReached  bool
	InboxEventsLimitReached  bool
	DeliveriesLimitReached   bool
}

type Cleaner interface {
	Cleanup(context.Context, Policy) (Result, error)
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Cleanup removes replay rows past expiry+safety-margin and terminal webhook
// payload rows past their retention windows. One transaction-level advisory
// lock serializes a bounded cleanup pass across worker replicas.
func (s *Store) Cleanup(ctx context.Context, policy Policy) (Result, error) {
	if s == nil || s.pool == nil {
		return Result{}, errors.New("cleanup store is not configured")
	}
	if err := validatePolicy(policy); err != nil {
		return Result{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result := Result{}
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, cleanupAdvisoryLockID).
		Scan(&result.LockAcquired); err != nil {
		return Result{}, fmt.Errorf("acquire cleanup advisory lock: %w", err)
	}
	if !result.LockAcquired {
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit skipped cleanup: %w", err)
		}
		return result, nil
	}

	result.WidgetTokens, result.WidgetTokensLimitReached, err = deleteExpired(
		ctx, tx, "used_widget_tokens", policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.IdempotencyKeys, result.IdempotencyLimitReached, err = deleteExpired(
		ctx, tx, "idempotency_keys", policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.InboxEvents, result.InboxEventsLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT ctid
				FROM inbox_events
				WHERE status IN ('processed', 'failed', 'dead', 'ignored')
				  AND updated_at < now() - ($1 * interval '1 millisecond')
				ORDER BY updated_at, ctid
				FOR UPDATE SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM inbox_events AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"inbox_events", policy.WebhookInboxRetention, policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.WebhookDeliveries, result.DeliveriesLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT delivery.ctid
				FROM webhook_deliveries AS delivery
				WHERE delivery.parse_status IN ('parsed', 'invalid', 'failed')
				  AND delivery.updated_at < now() - ($1 * interval '1 millisecond')
				  AND NOT EXISTS (
					SELECT 1 FROM inbox_events AS event
					WHERE event.delivery_id = delivery.id
				  )
				ORDER BY delivery.updated_at, delivery.ctid
				FOR UPDATE SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM webhook_deliveries AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"webhook_deliveries", policy.WebhookDeliveryRetention, policy,
	)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit cleanup: %w", err)
	}
	return result, nil
}

func deleteExpired(ctx context.Context, tx pgx.Tx, table string, policy Policy) (int64, bool, error) {
	query := `
		WITH victims AS (
			SELECT ctid
			FROM ` + table + `
			WHERE expires_at < now() - ($1 * interval '1 millisecond')
			ORDER BY expires_at, ctid
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		DELETE FROM ` + table + ` AS expired
		USING victims
		WHERE expired.ctid = victims.ctid`

	return deleteRetained(ctx, tx, query, table, policy.SafetyMargin, policy)
}

func deleteRetained(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	table string,
	retention time.Duration,
	policy Policy,
) (int64, bool, error) {
	var deleted int64
	for batch := 0; batch < policy.MaxBatches; batch++ {
		tag, err := tx.Exec(ctx, query, retention.Milliseconds(), policy.BatchSize)
		if err != nil {
			return 0, false, fmt.Errorf("delete retained %s: %w", table, err)
		}
		count := tag.RowsAffected()
		deleted += count
		if count < int64(policy.BatchSize) {
			return deleted, false, nil
		}
	}
	return deleted, true, nil
}

func validatePolicy(policy Policy) error {
	if policy.SafetyMargin < 0 {
		return errors.New("cleanup safety margin must not be negative")
	}
	if policy.BatchSize < 1 {
		return errors.New("cleanup batch size must be positive")
	}
	if policy.MaxBatches < 1 {
		return errors.New("cleanup maximum batches must be positive")
	}
	if policy.WebhookInboxRetention <= 0 {
		return errors.New("webhook inbox retention must be positive")
	}
	if policy.WebhookDeliveryRetention <= 0 {
		return errors.New("webhook delivery retention must be positive")
	}
	return nil
}

type SchedulerConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	Policy   Policy
}

type Scheduler struct {
	cleaner Cleaner
	logger  *slog.Logger
	config  SchedulerConfig
	metrics *Metrics
}

func NewScheduler(
	cleaner Cleaner,
	logger *slog.Logger,
	config SchedulerConfig,
	metricSets ...*Metrics,
) (*Scheduler, error) {
	if cleaner == nil {
		return nil, errors.New("cleanup scheduler cleaner is nil")
	}
	if logger == nil {
		return nil, errors.New("cleanup scheduler logger is nil")
	}
	if config.Interval <= 0 {
		return nil, errors.New("cleanup interval must be positive")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("cleanup timeout must be positive")
	}
	if err := validatePolicy(config.Policy); err != nil {
		return nil, err
	}
	var metrics *Metrics
	if len(metricSets) > 0 {
		metrics = metricSets[0]
	}
	return &Scheduler{cleaner: cleaner, logger: logger, config: config, metrics: metrics}, nil
}

// Run performs one startup pass and then runs periodically until cancellation.
// Individual cleanup failures are logged and retried on the next interval.
func (s *Scheduler) Run(ctx context.Context) error {
	s.runOnce(ctx)
	if ctx.Err() != nil {
		return nil
	}

	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(parent context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, s.config.Timeout)
	defer cancel()
	result, err := s.cleaner.Cleanup(ctx, s.config.Policy)
	s.metrics.observe(started, result, err)
	if err != nil {
		if parent.Err() == nil {
			s.logger.Error("cleanup pass failed", "error", err)
		}
		return
	}
	if !result.LockAcquired {
		s.logger.Debug("cleanup pass skipped", "reason", "lock_not_acquired")
		return
	}
	s.logger.Info("cleanup pass completed",
		"used_widget_tokens", result.WidgetTokens,
		"idempotency_keys", result.IdempotencyKeys,
	)
}
