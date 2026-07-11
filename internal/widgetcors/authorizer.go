// Package widgetcors provides a strict CORS boundary for browser calls made
// by amoCRM widgets. CORS is not authentication; widget JWT validation remains
// authoritative for tenant and actor identity.
package widgetcors

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// OriginAuthorizer decides whether an HTTPS browser origin belongs to at
// least one currently active installation of an active integration.
type OriginAuthorizer interface {
	IsActiveOrigin(context.Context, string) (bool, error)
}

// PostgresAuthorizer uses installations as the durable CORS allowlist.
type PostgresAuthorizer struct {
	pool *pgxpool.Pool
}

func NewPostgresAuthorizer(pool *pgxpool.Pool) *PostgresAuthorizer {
	return &PostgresAuthorizer{pool: pool}
}

func (a *PostgresAuthorizer) IsActiveOrigin(ctx context.Context, origin string) (bool, error) {
	if a == nil || a.pool == nil {
		return false, fmt.Errorf("widget CORS authorizer is not configured")
	}
	normalized, err := widgetauth.NormalizeHTTPSOrigin(origin)
	if err != nil || normalized != origin {
		return false, nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return false, nil
	}

	var active bool
	err = a.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM installations AS installation
			JOIN integrations AS integration
			  ON integration.id=installation.integration_id
			WHERE lower(installation.account_domain)=lower($1)
			  AND installation.status='active'
			  AND integration.status='active'
		)`, parsed.Host).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("authorize widget browser origin: %w", err)
	}
	return active, nil
}
