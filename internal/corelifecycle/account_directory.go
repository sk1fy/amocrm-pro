package corelifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// ListAccountInstallations lists only approved catalog bindings. An unknown integration
// cannot gain observation authority by sharing a domain or product name.
func (s *AccountObservationService) ListAccountInstallations(ctx context.Context, account int64, after uuid.UUID) ([]serviceapi.AccountInstallation, uuid.UUID, error) {
	if account <= 0 {
		return nil, uuid.Nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid account")
	}
	rows, err := s.store.pool.Query(ctx, `
 SELECT i.id,i.integration_id,COALESCE(m.product_code,b.product_code),n.status,i.status,
   EXISTS(SELECT FROM oauth_credentials o WHERE o.installation_id=i.id),
   m.product_code IS NULL,i.created_at,COALESCE(c.sequence,0)
 FROM installations i JOIN integrations n ON n.id=i.integration_id
 LEFT JOIN platform_lifecycle_cursors c ON c.installation_id=i.id
 LEFT JOIN platform_lifecycle_scope p ON p.account_id=i.account_id AND p.integration_id=i.integration_id
 LEFT JOIN platform_lifecycle_integrations m ON m.integration_id=p.integration_id
 LEFT JOIN platform_observation_bindings b ON b.installation_id=i.id AND b.integration_id=i.integration_id AND b.amo_account_id=i.account_id
 WHERE i.account_id=$1 AND i.id>$2 AND COALESCE(m.product_code,b.product_code) IS NOT NULL
 ORDER BY i.id LIMIT 101`, account, after)
	if err != nil {
		return nil, uuid.Nil, err
	}
	defer rows.Close()
	var items []serviceapi.AccountInstallation
	for rows.Next() {
		var item serviceapi.AccountInstallation
		var integration, installation string
		var credentials bool
		if err := rows.Scan(&item.Installation, &item.Integration, &item.ProductCode, &integration, &installation, &credentials, &item.ObservationOnly, &item.RegisteredAt, &item.LifecycleSequence); err != nil {
			return nil, uuid.Nil, err
		}
		auth := EffectiveAuthorization(integration, installation)
		if auth == wire.Authorized && !credentials {
			auth = wire.Unknown
		}
		item.Authorization = string(auth)
		item.Current = installation != "uninstalled"
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, uuid.Nil, err
	}
	if len(items) > 100 {
		return items[:100], items[99].Installation, nil
	}
	return items, uuid.Nil, nil
}

func (s *AccountObservationService) legacyObservationAuthority(ctx context.Context, scope serviceapi.AccountObservationScope) (platformInstallationAuthority, error) {
	var a platformInstallationAuthority
	err := s.store.pool.QueryRow(ctx, `
 SELECT b.product_code,i.account_domain,i.updated_at FROM platform_observation_bindings b
 JOIN installations i ON i.id=b.installation_id AND i.integration_id=b.integration_id AND i.account_id=b.amo_account_id
 JOIN integrations n ON n.id=i.integration_id
 WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3 AND i.status='active' AND n.status='active'
 AND EXISTS(SELECT FROM oauth_credentials o WHERE o.installation_id=i.id)
 AND NOT EXISTS(SELECT FROM platform_lifecycle_scope p WHERE p.integration_id=i.integration_id AND p.account_id=i.account_id)`, scope.Installation, scope.Integration, scope.Account).Scan(&a.productCode, &a.domain, &a.observedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, serviceapi.Fail(serviceapi.PermissionDenied, "account observation binding unavailable")
	}
	if err != nil {
		return a, fmt.Errorf("check account observation binding: %w", err)
	}
	return a, nil
}
