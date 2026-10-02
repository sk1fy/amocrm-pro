package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

const LeadStatusConsumer = "core-lead-status-v1"
const DistributionConsumer = "core-lead-distribution-v1"

// ConsumerDescriptor declares dispatch ownership; inline preserves the legacy
// synchronous route transaction, other consumers own independent durable jobs.
type ConsumerDescriptor struct {
	ID, JobType string
	Inline      bool
}
type ConsumerSnapshot struct {
	ConsumerID string          `json:"consumerId"`
	Scope      json.RawMessage `json:"scope"`
}
type FrozenConsumer struct {
	ReceiptID  uuid.UUID       `json:"receiptId"`
	ConsumerID string          `json:"consumerId"`
	Scope      json.RawMessage `json:"scope"`
	Event      services.Event  `json:"event"`
}

func (s *Store) RegisterEventConsumer(entity, event string, d ConsumerDescriptor) {
	if d.ID != LeadStatusConsumer && d.ID != DistributionConsumer {
		panic("unknown event consumer")
	}
	if d.JobType == "" {
		panic("consumer job descriptor missing")
	}
	key := entity + ":" + event
	if s.consumers[key] == nil {
		s.consumers[key] = map[string]ConsumerDescriptor{}
	}
	if _, exists := s.consumers[key][d.ID]; exists {
		panic("duplicate event consumer")
	}
	s.consumers[key][d.ID] = d
}
func (s *Store) ConsumerEvent(ctx context.Context, receiptID, installationID uuid.UUID) (FrozenConsumer, error) {
	var frozen FrozenConsumer
	var raw []byte
	e := s.pool.QueryRow(ctx, `SELECT frozen_payload FROM webhook_consumer_receipts WHERE id=$1 AND installation_id=$2`, receiptID, installationID).Scan(&raw)
	if errors.Is(e, pgx.ErrNoRows) {
		return frozen, ErrNotFound
	}
	if e != nil {
		return frozen, e
	}
	e = json.Unmarshal(raw, &frozen)
	if e == nil && (frozen.ReceiptID != receiptID || frozen.Event.InstallationID != installationID) {
		return FrozenConsumer{}, ErrNotFound
	}
	return frozen, e
}
func (s *Store) freezeConsumers(ctx context.Context, tx pgx.Tx, d Delivery, e Event, id uuid.UUID, raw []byte) error {
	var members []ConsumerSnapshot
	if raw == nil {
		members = []ConsumerSnapshot{{ConsumerID: LeadStatusConsumer}}
	} else if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for _, m := range members {
		descriptor, registered := s.consumers[e.EntityType+":"+e.EventType][m.ConsumerID]
		if !registered {
			if m.ConsumerID == LeadStatusConsumer {
				descriptor = ConsumerDescriptor{ID: LeadStatusConsumer, JobType: "webhook.process_event", Inline: true}
			} else if e.EntityType == "leads" {
				return errors.New("enabled consumer is not registered")
			}
		}

		if m.ConsumerID == DistributionConsumer && e.EntityType != "leads" {
			continue
		}
		receipt := uuid.New()
		frozen := FrozenConsumer{ReceiptID: receipt, ConsumerID: m.ConsumerID, Scope: m.Scope, Event: services.Event{ID: id, InstallationID: d.InstallationID, EntityType: e.EntityType, EventType: e.EventType, EntityID: e.EntityID, Payload: receiptPayload(e.Payload), DeduplicationKey: e.DeduplicationKey, ReceivedAt: d.ReceivedAt, EventAt: e.EventAt}}
		body, err := json.Marshal(frozen)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO webhook_consumer_receipts(id,event_id,source_inbox_event_id,installation_id,consumer_id,source_fingerprint,frozen_payload) VALUES($1,$2,$2,$3,$4,$5,$6) ON CONFLICT(installation_id,consumer_id,source_fingerprint) DO NOTHING`, receipt, id, d.InstallationID, m.ConsumerID, e.DeduplicationKey, body)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if descriptor.Inline {
			continue
		} // Existing synchronous consumer owns its existing job.
		payload, _ := json.Marshal(map[string]any{"receiptId": receipt})
		var job uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jobs(installation_id,type,priority,payload) VALUES($1,$3,20,$2) RETURNING id`, d.InstallationID, payload, descriptor.JobType).Scan(&job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_consumer_receipts SET job_id=$2 WHERE id=$1`, receipt, job); err != nil {
			return err
		}
	}
	return nil
}

// Durable receipts retain only routing/source evidence, never lead names or
// arbitrary custom fields from raw webhook bodies.
func receiptPayload(raw json.RawMessage) json.RawMessage {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return json.RawMessage(`{}`)
	}
	keep := map[string]any{}
	for _, key := range []string{"id", "status_id", "old_status_id", "pipeline_id", "old_pipeline_id", "responsible_user_id", "old_responsible_user_id", "last_modified", "updated_at", "created_at", "date_create", "timestamp"} {
		if value, ok := fields[key]; ok {
			keep[key] = value
		}
	}
	out, _ := json.Marshal(keep)
	return out
}
