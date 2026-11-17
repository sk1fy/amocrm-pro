package corelifecycle

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

var ErrAccessProofDenied = errors.New("Core installation is not authorized for access proof")
var ErrAccessProofPolicy = errors.New("Core installation has no unambiguous access proof policy")

func (s *Store) AccessProofScope(ctx context.Context, principal widgetauth.Principal) (platformaccess.Scope, error) {
	scope := platformaccess.Scope{
		Account: strconv.FormatInt(principal.AccountID, 10), Integration: principal.IntegrationID,
		Installation: principal.InstallationID,
	}
	var product *string
	var unambiguous, authorized bool
	err := s.pool.QueryRow(ctx, `
		SELECT m.product_code,
			p.account_id IS NOT NULL
				AND EXISTS(SELECT 1 FROM platform_lifecycle_manifest WHERE version=1)
				AND NOT EXISTS(
					SELECT 1 FROM platform_lifecycle_integrations other
					JOIN platform_lifecycle_scope approved USING(integration_id)
					WHERE other.product_code=m.product_code AND approved.account_id=i.account_id
						AND other.integration_id<>i.integration_id),
			COALESCE(NOT c.closed, false)
				AND EXISTS(SELECT 1 FROM oauth_credentials o WHERE o.installation_id=i.id),
			COALESCE(c.generation, 0), COALESCE(c.authorization_sequence, 0)
		FROM installations i
		JOIN integrations n ON n.id=i.integration_id
		LEFT JOIN platform_lifecycle_integrations m ON m.integration_id=i.integration_id
		LEFT JOIN platform_lifecycle_scope p ON p.integration_id=i.integration_id AND p.account_id=i.account_id
		LEFT JOIN platform_lifecycle_cursors c ON c.installation_id=i.id
		WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3
			AND n.status='active' AND i.status='active'`,
		principal.InstallationID, principal.IntegrationID, principal.AccountID).
		Scan(&product, &unambiguous, &authorized, &scope.Generation, &scope.AuthorizationSequence)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return platformaccess.Scope{}, ErrAccessProofDenied
	case err != nil:
		return platformaccess.Scope{}, err
	case product == nil || !unambiguous:
		return platformaccess.Scope{}, ErrAccessProofPolicy
	case !authorized:
		return platformaccess.Scope{}, ErrAccessProofDenied
	}
	scope.Product = *product
	return scope, nil
}
