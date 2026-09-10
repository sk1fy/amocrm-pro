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

// RedeliveryHorizon is the Core calendar floor for command identity, terminal
// jobs, and aligned technical history. It matches ADR-0016 HistoryHorizon.
const RedeliveryHorizon = 7 * 24 * time.Hour

// TombstoneRetention is the default last_seen_at TTL for webhook replay
// identity. It is longer than raw payload retention so a collected delivery
// cannot become actionable again. Non-commutative future workflows must not
// assume this window is eternal.
const TombstoneRetention = 90 * 24 * time.Hour

type Policy struct {
	SafetyMargin             time.Duration
	WebhookInboxRetention    time.Duration
	WebhookDeliveryRetention time.Duration
	RedeliveryHorizon        time.Duration
	TombstoneRetention       time.Duration
	BatchSize                int
	MaxBatches               int
}

type Result struct {
	LockAcquired                   bool
	WidgetTokens                   int64
	IdempotencyKeys                int64
	OAuthStates                    int64
	InboxEvents                    int64
	WebhookDeliveries              int64
	OutboundEffects                int64
	WorkflowRuns                   int64
	RuleConfigurations             int64
	Jobs                           int64
	Tombstones                     int64
	Audit                          int64
	CommandReceipts                int64
	WidgetTokensLimitReached       bool
	IdempotencyLimitReached        bool
	OAuthStatesLimitReached        bool
	InboxEventsLimitReached        bool
	DeliveriesLimitReached         bool
	OutboundEffectsLimitReached    bool
	WorkflowRunsLimitReached       bool
	RuleConfigurationsLimitReached bool
	JobsLimitReached               bool
	TombstonesLimitReached         bool
	AuditLimitReached              bool
	CommandReceiptsLimitReached    bool
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

// Cleanup removes replay rows past expiry+safety-margin, terminal webhook
// payload rows past their retention windows, and Core technical history past
// the calendar redelivery horizon. One transaction-level advisory lock
// serializes a bounded cleanup pass across worker replicas. prepared/applied/
// uncertain outbound effects are kept.
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
	// Keep consumed and unused states until expiry plus the safety margin.
	// Callback state consumption is an atomic database operation; SKIP LOCKED
	// avoids contending with a callback currently consuming a state.
	result.OAuthStates, result.OAuthStatesLimitReached, err = deleteExpired(
		ctx, tx, "oauth_states", policy,
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
	horizon := effectiveHorizon(policy)
	if err := expireAgedDeliveries(ctx, tx, horizon, policy); err != nil {
		return Result{}, err
	}
	// Observed/no_effect/failed/expired effects have a known outcome. prepared,
	// applied, and uncertain rows stay: the remote mutation may still be in
	// flight or unknown (CORE-02 reconcile).
	result.OutboundEffects, result.OutboundEffectsLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT effect.ctid
				FROM outbound_effects AS effect
				WHERE effect.state IN ('observed', 'no_effect', 'failed', 'expired')
				  AND effect.updated_at < now() - ($1 * interval '1 millisecond')
				ORDER BY effect.updated_at, effect.ctid
				FOR UPDATE OF effect SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM outbound_effects AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"outbound_effects", horizon, policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.WorkflowRuns, result.WorkflowRunsLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT run.ctid
				FROM workflow_runs AS run
				WHERE run.status IN ('completed', 'failed', 'dead')
				  AND run.finished_at IS NOT NULL
				  AND run.finished_at < now() - ($1 * interval '1 millisecond')
				  AND NOT EXISTS (
					SELECT 1 FROM outbound_effects AS effect
					WHERE effect.workflow_run_id = run.id
				  )
				ORDER BY run.finished_at, run.ctid
				FOR UPDATE OF run SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM workflow_runs AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"workflow_runs", horizon, policy,
	)
	if err != nil {
		return Result{}, err
	}
	// Configuration results belong to their completed job. Preserve them while
	// any idempotency receipt or unknown effect still needs the original result.
	result.RuleConfigurations, result.RuleConfigurationsLimitReached, err = deleteRetained(ctx, tx, `
		WITH victims AS (
			SELECT config.job_id FROM lead_status_workflow_rule_configurations config
			JOIN jobs job ON job.id=config.job_id
			WHERE config.configured_at < now()-($1*interval '1 millisecond')
			  AND job.status IN ('completed','failed','dead','cancelled')
			  AND job.finished_at < now()-($1*interval '1 millisecond')
			  AND NOT EXISTS (SELECT 1 FROM idempotency_keys k WHERE k.job_id=job.id)
			  AND NOT EXISTS (SELECT 1 FROM outbound_effects e WHERE e.correlation_job_id=job.id)
			ORDER BY config.configured_at,config.job_id
			FOR UPDATE OF config,job SKIP LOCKED LIMIT $2
		) DELETE FROM lead_status_workflow_rule_configurations config USING victims
		WHERE config.job_id=victims.job_id`, "rule_configurations", horizon, policy)
	if err != nil {
		return Result{}, err
	}
	result.Jobs, result.JobsLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT job.ctid
				FROM jobs AS job
				WHERE job.status IN ('completed', 'failed', 'dead', 'cancelled')
				  AND job.finished_at IS NOT NULL
				  AND job.finished_at < now() - ($1 * interval '1 millisecond')
				  AND NOT EXISTS (
					SELECT 1 FROM outbound_effects AS effect
					WHERE effect.correlation_job_id = job.id
				  )
				  AND NOT EXISTS (SELECT 1 FROM idempotency_keys k WHERE k.job_id=job.id)
				  AND NOT EXISTS (SELECT 1 FROM lead_status_workflow_rule_configurations c WHERE c.job_id=job.id)
				ORDER BY job.finished_at, job.ctid
				FOR UPDATE OF job SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM jobs AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"jobs", horizon, policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.Tombstones, result.TombstonesLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT tombstone.ctid
				FROM webhook_event_tombstones AS tombstone
				WHERE tombstone.last_seen_at < now() - ($1 * interval '1 millisecond')
				ORDER BY tombstone.last_seen_at, tombstone.ctid
				FOR UPDATE OF tombstone SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM webhook_event_tombstones AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"webhook_event_tombstones", effectiveTombstoneRetention(policy), policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.Audit, result.AuditLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT entry.ctid
				FROM audit_log AS entry
				WHERE entry.created_at < now() - ($1 * interval '1 millisecond')
				ORDER BY entry.created_at, entry.ctid
				FOR UPDATE OF entry SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM audit_log AS expired
			USING victims
			WHERE expired.ctid = victims.ctid`,
		"audit_log", horizon, policy,
	)
	if err != nil {
		return Result{}, err
	}
	result.CommandReceipts, result.CommandReceiptsLimitReached, err = deleteRetained(
		ctx, tx, `
			WITH victims AS (
				SELECT receipt.command_id
				FROM activity_command_receipts AS receipt
				JOIN activity_command_outbox AS outbox USING (command_id)
				WHERE outbox.status IN ('accepted', 'failed', 'expired')
				  AND receipt.created_at < now() - ($1 * interval '1 millisecond')
				ORDER BY receipt.created_at, receipt.command_id
				FOR UPDATE OF receipt, outbox SKIP LOCKED
				LIMIT $2
			)
			DELETE FROM activity_command_receipts AS expired
			USING victims
			WHERE expired.command_id = victims.command_id`,
		"activity_command_receipts", horizon, policy,
	)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit cleanup: %w", err)
	}
	return result, nil
}

func expireAgedDeliveries(ctx context.Context, tx pgx.Tx, horizon time.Duration, policy Policy) error {
	query := `
		WITH expired AS (
			SELECT outbox.ctid
			FROM activity_command_outbox AS outbox
			JOIN activity_command_receipts AS receipt USING (command_id)
			WHERE receipt.created_at < now() - ($1 * interval '1 millisecond')
			  AND (
				outbox.status = 'pending_delivery'
				OR (outbox.status = 'delivering' AND outbox.leased_until < now())
			  )
			ORDER BY receipt.created_at, outbox.ctid
			FOR UPDATE OF outbox SKIP LOCKED
			LIMIT $2
		)
		UPDATE activity_command_outbox AS outbox
		SET status = 'expired', error_code = 'delivery_expired',
			lease_token = NULL, leased_until = NULL, updated_at = now()
		FROM expired
		WHERE outbox.ctid = expired.ctid`
	for batch := 0; batch < policy.MaxBatches; batch++ {
		tag, err := tx.Exec(ctx, query, horizon.Milliseconds(), policy.BatchSize)
		if err != nil {
			return fmt.Errorf("expire aged deliveries: %w", err)
		}
		if tag.RowsAffected() < int64(policy.BatchSize) {
			return nil
		}
	}
	return nil
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
	if policy.RedeliveryHorizon < 0 {
		return errors.New("redelivery horizon must not be negative")
	}
	if policy.TombstoneRetention < 0 {
		return errors.New("tombstone retention must not be negative")
	}
	return nil
}

func effectiveHorizon(policy Policy) time.Duration {
	if policy.RedeliveryHorizon >= RedeliveryHorizon {
		return policy.RedeliveryHorizon
	}
	return RedeliveryHorizon
}

func effectiveTombstoneRetention(policy Policy) time.Duration {
	retention := policy.TombstoneRetention
	if retention <= 0 {
		retention = TombstoneRetention
	}
	if retention < policy.WebhookInboxRetention {
		retention = policy.WebhookInboxRetention
	}
	if retention < RedeliveryHorizon {
		retention = RedeliveryHorizon
	}
	return retention
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
		"oauth_states", result.OAuthStates,
		"jobs", result.Jobs,
		"command_receipts", result.CommandReceipts,
	)
}
