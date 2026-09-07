package activitybridge

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

// A database failure does not prove that the capability was revoked.
func requireActivityEnabled(ctx context.Context, db services.Querier, installation uuid.UUID) error {
	err := services.RequireEnabled(ctx, db, installation, services.Activity, true)
	if err == nil {
		return nil
	}
	if errors.Is(err, services.ErrNotEnabled) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "activity is not enabled")
	}
	return serviceapi.Fail(serviceapi.Unavailable, "activity admission is unavailable")
}
