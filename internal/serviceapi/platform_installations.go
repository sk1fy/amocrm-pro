package serviceapi

import (
	"context"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type CoreInstallationSnapshot struct {
	InstallationID        uuid.UUID
	IntegrationID         uuid.UUID
	AccountID             int64
	Domain                *string
	Authorization         wire.Authorization
	ObservedAt            time.Time
	Generation            *uint64
	Sequence              *uint64
	AuthorizationSequence *uint64
}

type CoreInstallations interface {
	GetSnapshot(context.Context, string, uuid.UUID) (CoreInstallationSnapshot, error)
}
