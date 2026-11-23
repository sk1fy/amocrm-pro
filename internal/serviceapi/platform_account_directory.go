package serviceapi

import (
	"context"
	"github.com/google/uuid"
	"time"
)

type AccountInstallation struct {
	Installation, Integration  uuid.UUID
	ProductCode, Authorization string
	Current, ObservationOnly   bool
	LifecycleSequence          int64
	RegisteredAt               time.Time
}

type CoreAccountDirectory interface {
	ListAccountInstallations(context.Context, int64, uuid.UUID) ([]AccountInstallation, uuid.UUID, error)
}
