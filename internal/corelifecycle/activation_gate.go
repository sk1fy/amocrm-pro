package corelifecycle

import (
	"context"
	"errors"

	"github.com/google/uuid"

	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// The confirmation belongs to one installation generation. Reinstalling opens a
// new generation; a review or a previous generation's phone cannot grant access.
func (s *Store) CanUseWidget(ctx context.Context, p widgetauth.Principal, c *CatalogClient, a *ActivationClient) (bool, error) {
	var scoped, confirmed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_lifecycle_integrations WHERE integration_id=$1),
 EXISTS(SELECT FROM widget_activation_confirmations a JOIN platform_lifecycle_cursors c USING(installation_id)
 WHERE a.installation_id=$2 AND c.generation=a.generation AND NOT c.closed)`, p.IntegrationID, p.InstallationID).Scan(&scoped, &confirmed)
	if err != nil || confirmed {
		return confirmed, err
	}
	if !scoped {
		if c == nil {
			return true, nil
		}
		scoped, err = c.Managed(ctx, p.IntegrationID)
		if err != nil || !scoped {
			return !scoped && err == nil, err
		}
	}
	if a == nil {
		return false, errors.New("Platform activation unavailable")
	}
	scope, err := s.AccessProofScope(ctx, p)
	if errors.Is(err, ErrAccessProofPolicy) || errors.Is(err, ErrAccessProofDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := a.Apply(ctx, wire.ActivationRequest{Scope: wire.ActivationScope{Installation: scope.Installation, Integration: scope.Integration, Account: p.AccountID, Generation: scope.Generation, AuthorizationSequence: scope.AuthorizationSequence}})
	if errors.Is(err, ErrActivationUnavailable) {
		return false, nil
	}
	if err != nil || !result.Activated {
		return false, err
	}
	err = s.ConfirmActivation(ctx, p.InstallationID, scope.Generation)
	return err == nil, err
}

func (s *Store) ConfirmActivation(ctx context.Context, id uuid.UUID, generation int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO widget_activation_confirmations AS a(installation_id,generation) VALUES($1,$2)
 ON CONFLICT(installation_id) DO UPDATE SET generation=excluded.generation WHERE a.generation<excluded.generation`, id, generation)
	return err
}
