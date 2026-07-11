package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

type Delivery struct {
	ID             uuid.UUID
	InstallationID uuid.UUID
	RequestID      uuid.UUID
	ContentType    string
	RawBody        []byte
	ParseStatus    string
	ReceivedAt     time.Time
	WebhookEvents  []string
}

type Store struct {
	pool    *pgxpool.Pool
	metrics *Metrics
}

var ErrNotFound = errors.New("webhook object not found in installation scope")

func NewStore(pool *pgxpool.Pool, metricSets ...*Metrics) *Store {
	var metrics *Metrics
	if len(metricSets) > 0 {
		metrics = metricSets[0]
	}
	return &Store{pool: pool, metrics: metrics}
}

func (s *Store) SaveDeliveryAndEnqueue(
	ctx context.Context,
	installationID uuid.UUID,
	requestID uuid.UUID,
	contentType string,
	rawBody []byte,
) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin webhook ingress: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	bodyHash := sha256.Sum256(rawBody)
	var deliveryID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (
			installation_id, request_id, content_type, event_settings, raw_body, body_sha256
		)
		SELECT installation.id, $2, $3, installation.webhook_settings, $4, $5
		FROM installations installation
		WHERE installation.id = $1
		RETURNING id`,
		installationID, requestID, contentType, rawBody, bodyHash[:],
	).Scan(&deliveryID); err != nil {
		return uuid.Nil, fmt.Errorf("save webhook delivery: %w", err)
	}

	payload, err := json.Marshal(map[string]string{"delivery_id": deliveryID.String()})
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal webhook parse job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (installation_id, type, priority, payload)
		VALUES ($1, 'webhook.parse', 10, $2)`, installationID, payload); err != nil {
		return uuid.Nil, fmt.Errorf("enqueue webhook parse job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit webhook ingress: %w", err)
	}
	return deliveryID, nil
}

func (s *Store) SaveInvalidDelivery(
	ctx context.Context,
	installationID uuid.UUID,
	requestID uuid.UUID,
	contentType string,
	rawBody []byte,
	parseError string,
) (uuid.UUID, error) {
	bodyHash := sha256.Sum256(rawBody)
	var deliveryID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (
			installation_id, request_id, content_type, event_settings, raw_body, body_sha256,
			parse_status, parse_error, parsed_at
		)
		SELECT installation.id, $2, $3, installation.webhook_settings, $4, $5,
			'invalid', $6, now()
		FROM installations installation
		WHERE installation.id = $1
		RETURNING id`,
		installationID, requestID, contentType, rawBody, bodyHash[:], sanitize.Text(parseError, 4000),
	).Scan(&deliveryID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("save invalid webhook delivery: %w", err)
	}
	return deliveryID, nil
}

func (s *Store) GetDelivery(ctx context.Context, id, installationID uuid.UUID) (Delivery, error) {
	var delivery Delivery
	var configuredEvents json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT delivery.id, delivery.installation_id, delivery.request_id,
			delivery.content_type, delivery.raw_body, delivery.parse_status,
			delivery.received_at, delivery.event_settings
		FROM webhook_deliveries delivery
		WHERE delivery.id = $1 AND delivery.installation_id = $2`, id, installationID,
	).Scan(
		&delivery.ID,
		&delivery.InstallationID,
		&delivery.RequestID,
		&delivery.ContentType,
		&delivery.RawBody,
		&delivery.ParseStatus,
		&delivery.ReceivedAt,
		&configuredEvents,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("get webhook delivery: %w", err)
	}
	if err := json.Unmarshal(configuredEvents, &delivery.WebhookEvents); err != nil {
		return Delivery{}, fmt.Errorf("decode configured webhook events: %w", err)
	}
	return delivery, nil
}

func (s *Store) MarkInvalid(ctx context.Context, id uuid.UUID, parseError string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries
		SET parse_status = 'invalid', parse_error = $2, parsed_at = now(), updated_at = now()
		WHERE id = $1 AND parse_status IN ('pending', 'processing')`, id, sanitize.Text(parseError, 4000),
	)
	if err != nil {
		return fmt.Errorf("mark webhook delivery invalid: %w", err)
	}
	return nil
}

func (s *Store) SaveParsedEvents(ctx context.Context, delivery Delivery, events []Event) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin save webhook events: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT parse_status FROM webhook_deliveries WHERE id = $1 FOR UPDATE`, delivery.ID,
	).Scan(&status); err != nil {
		return 0, fmt.Errorf("lock webhook delivery: %w", err)
	}
	if status == "parsed" || status == "invalid" {
		return 0, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries SET parse_status = 'processing', updated_at = now() WHERE id = $1`, delivery.ID,
	); err != nil {
		return 0, fmt.Errorf("mark webhook delivery processing: %w", err)
	}

	inserted := 0
	for _, event := range events {
		tombstone, err := tx.Exec(ctx, `
			INSERT INTO webhook_event_tombstones (
				installation_id, deduplication_key
			) VALUES ($1, $2)
			ON CONFLICT (installation_id, deduplication_key) DO NOTHING`,
			delivery.InstallationID, event.DeduplicationKey,
		)
		if err != nil {
			return 0, fmt.Errorf("claim webhook event tombstone: %w", err)
		}
		if tombstone.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_event_tombstones SET last_seen_at=GREATEST(last_seen_at, now())
				WHERE installation_id=$1 AND deduplication_key=$2`,
				delivery.InstallationID, event.DeduplicationKey,
			); err != nil {
				return 0, fmt.Errorf("touch webhook event tombstone: %w", err)
			}
			continue
		}
		var eventID uuid.UUID
		err = tx.QueryRow(ctx, `
			INSERT INTO inbox_events (
				delivery_id, installation_id, entity_type, event_type,
				entity_id, event_at, payload, deduplication_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id`,
			delivery.ID,
			delivery.InstallationID,
			event.EntityType,
			event.EventType,
			event.EntityID,
			event.EventAt,
			event.Payload,
			event.DeduplicationKey,
		).Scan(&eventID)
		if err != nil {
			return 0, fmt.Errorf("save inbox event: %w", err)
		}

		payload, err := json.Marshal(map[string]string{"event_id": eventID.String()})
		if err != nil {
			return 0, fmt.Errorf("marshal webhook event job: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO jobs (installation_id, type, priority, payload)
			VALUES ($1, 'webhook.process_event', 20, $2)`, delivery.InstallationID, payload); err != nil {
			return 0, fmt.Errorf("enqueue webhook event job: %w", err)
		}
		inserted++
	}

	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries
		SET parse_status = 'parsed', parse_error = NULL, parsed_at = now(), updated_at = now()
		WHERE id = $1`, delivery.ID,
	); err != nil {
		return 0, fmt.Errorf("mark webhook delivery parsed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit webhook events: %w", err)
	}
	return inserted, nil
}

func (s *Store) ProcessEvent(ctx context.Context, eventID, expectedInstallationID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin process webhook event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var installationID uuid.UUID
	var entityType, eventType string
	var entityID *int64
	var payload json.RawMessage
	var deduplicationKey []byte
	var deliveryReceivedAt time.Time
	var status string
	err = tx.QueryRow(ctx, `
		SELECT event.installation_id, event.entity_type, event.event_type,
			event.entity_id, event.payload, event.deduplication_key,
			delivery.received_at, event.status
		FROM inbox_events AS event
		JOIN webhook_deliveries AS delivery ON delivery.id=event.delivery_id
		WHERE event.id = $1 AND event.installation_id = $2
		FOR UPDATE OF event`, eventID, expectedInstallationID,
	).Scan(
		&installationID, &entityType, &eventType, &entityID, &payload,
		&deduplicationKey, &deliveryReceivedAt, &status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock inbox event: %w", err)
	}
	if status == "processed" || status == "ignored" {
		return tx.Commit(ctx)
	}

	route, err := s.routeLeadStatusEvent(ctx, tx, leadStatusEvent{
		ID: eventID, InstallationID: installationID, EntityType: entityType,
		EventType: eventType, EntityID: entityID, Payload: payload,
		DeduplicationKey: deduplicationKey, ReceivedAt: deliveryReceivedAt,
	})
	if err != nil {
		return err
	}
	finalStatus := "processed"
	if route.EffectID != nil {
		finalStatus = "ignored"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE inbox_events
		SET status = $2, attempts = attempts + 1, processed_at = now(),
			correlated_effect_id=$3, updated_at = now()
		WHERE id = $1`, eventID, finalStatus, route.EffectID,
	); err != nil {
		return fmt.Errorf("mark inbox event processed: %w", err)
	}
	metadata, err := json.Marshal(map[string]any{
		"event_type":           eventType,
		"entity_type":          entityType,
		"entity_id":            entityID,
		"disposition":          route.Disposition,
		"workflow_run_id":      route.RunID,
		"correlated_effect_id": route.EffectID,
	})
	if err != nil {
		return fmt.Errorf("marshal webhook event audit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (
			installation_id, actor_type, action, object_type, object_id, metadata
		) VALUES ($1, 'integration', 'webhook.event.processed', 'inbox_event', $2, $3)`,
		installationID, eventID.String(), metadata,
	); err != nil {
		return fmt.Errorf("audit webhook event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.metrics.observeLeadStatusRoute(route.Disposition)
	return nil
}

func (s *Store) RecordJobFailure(
	ctx context.Context,
	tx jobs.TxExecutor,
	job jobs.Job,
	failure jobs.Failure,
	status jobs.Status,
) error {
	if job.InstallationID == nil {
		return nil
	}
	switch job.Type {
	case "webhook.parse":
		var payload deliveryPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil
		}
		deliveryID, err := uuid.Parse(payload.DeliveryID)
		if err != nil {
			return nil
		}
		parseStatus := "pending"
		terminal := false
		if status == jobs.StatusFailed || status == jobs.StatusDead {
			parseStatus = "failed"
			terminal = true
		}
		tag, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries
			SET parse_status = $3, parse_error = $4,
				parsed_at = CASE WHEN $5 THEN now() ELSE NULL END, updated_at = now()
			WHERE id = $1 AND installation_id = $2 AND parse_status NOT IN ('parsed', 'invalid')`,
			deliveryID, *job.InstallationID, parseStatus, sanitize.Text(failure.Message, 4000), terminal,
		)
		if err != nil {
			return fmt.Errorf("record webhook parse failure: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
	case "webhook.process_event":
		var payload eventPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil
		}
		eventID, err := uuid.Parse(payload.EventID)
		if err != nil {
			return nil
		}
		eventStatus := "pending"
		terminal := false
		if status == jobs.StatusFailed {
			eventStatus, terminal = "failed", true
		} else if status == jobs.StatusDead {
			eventStatus, terminal = "dead", true
		}
		tag, err := tx.Exec(ctx, `
			UPDATE inbox_events
			SET status = $3, attempts = GREATEST(attempts, $4),
				last_error = $5,
				processed_at = CASE WHEN $6 THEN now() ELSE NULL END, updated_at = now()
			WHERE id = $1 AND installation_id = $2 AND status NOT IN ('processed', 'ignored')`,
			eventID, *job.InstallationID, eventStatus, job.Attempts,
			sanitize.Text(failure.Message, 4000), terminal,
		)
		if err != nil {
			return fmt.Errorf("record webhook event failure: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
	case LeadStatusTransitionJobType:
		var payload leadStatusTransitionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.WorkflowRunID == uuid.Nil {
			return nil
		}
		runStatus := "queued"
		terminal := false
		if status == jobs.StatusFailed {
			runStatus, terminal = "failed", true
		} else if status == jobs.StatusDead {
			runStatus, terminal = "dead", true
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workflow_runs
			SET status=$4, finished_at=CASE WHEN $5 THEN now() ELSE NULL END
			WHERE id=$1 AND installation_id=$2 AND job_id=$3 AND status <> 'completed'`,
			payload.WorkflowRunID, *job.InstallationID, job.ID, runStatus, terminal,
		); err != nil {
			return fmt.Errorf("record workflow run failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE outbound_effects
			SET state=CASE
					WHEN state='prepared' THEN 'uncertain'
					ELSE state
				END,
				last_error=$2, updated_at=now()
			WHERE correlation_job_id=$1`,
			job.ID, sanitize.Text(failure.Message, 4000),
		); err != nil {
			return fmt.Errorf("record workflow effect failure: %w", err)
		}
	}
	return nil
}
