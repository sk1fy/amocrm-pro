package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

type deliveryPayload struct {
	DeliveryID string `json:"delivery_id"`
}

type eventPayload struct {
	EventID string `json:"event_id"`
}

func ParseJobHandler(store *Store) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if job.InstallationID == nil {
			return nil, jobs.Permanent("invalid_tenant_scope", errors.New("webhook parse job has no installation"))
		}
		var payload deliveryPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, jobs.Permanent("invalid_payload", fmt.Errorf("decode webhook parse payload: %w", err))
		}
		deliveryID, err := uuid.Parse(payload.DeliveryID)
		if err != nil {
			return nil, jobs.Permanent("invalid_payload", fmt.Errorf("invalid delivery id: %w", err))
		}
		delivery, err := store.GetDelivery(ctx, deliveryID, *job.InstallationID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, jobs.Permanent("tenant_scope_mismatch", err)
			}
			return nil, err
		}
		if delivery.ParseStatus == "parsed" || delivery.ParseStatus == "invalid" {
			return json.RawMessage(`{"status":"already_final"}`), nil
		}

		events, err := ParseAllowed(delivery.InstallationID, delivery.RawBody, delivery.WebhookEvents)
		if err != nil {
			if markErr := store.MarkInvalid(ctx, delivery.ID, err.Error()); markErr != nil {
				return nil, markErr
			}
			return json.RawMessage(`{"status":"invalid"}`), nil
		}
		inserted, err := store.SaveParsedEvents(ctx, delivery, events)
		if err != nil {
			return nil, err
		}
		result, _ := json.Marshal(map[string]int{"events": len(events), "inserted": inserted})
		return result, nil
	}
}

func ProcessEventJobHandler(store *Store) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if job.InstallationID == nil {
			return nil, jobs.Permanent("invalid_tenant_scope", errors.New("webhook event job has no installation"))
		}
		var payload eventPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, jobs.Permanent("invalid_payload", fmt.Errorf("decode webhook event payload: %w", err))
		}
		eventID, err := uuid.Parse(payload.EventID)
		if err != nil {
			return nil, jobs.Permanent("invalid_payload", fmt.Errorf("invalid event id: %w", err))
		}
		if err := store.ProcessEvent(ctx, eventID, *job.InstallationID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, jobs.Permanent("tenant_scope_mismatch", err)
			}
			return nil, err
		}
		return json.RawMessage(`{"status":"processed"}`), nil
	}
}

func JobFailureObserver(store *Store) jobs.FailureObserver {
	return store.RecordJobFailure
}
