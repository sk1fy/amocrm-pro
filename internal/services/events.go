package services

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event is a normalized, durably stored webhook event, scoped by trusted ingress.
type Event struct {
	ID               uuid.UUID
	InstallationID   uuid.UUID
	EntityType       string
	EventType        string
	EntityID         *int64
	Payload          json.RawMessage
	DeduplicationKey []byte
	ReceivedAt       time.Time
}
type EventRoute struct {
	Workflow    string
	Disposition string
	RunID       *uuid.UUID
	EffectID    *uuid.UUID
}

// EventRouter runs in the inbox transaction; receipt and enqueued work commit together.
type EventRouter func(context.Context, pgx.Tx, Event) (EventRoute, error)
