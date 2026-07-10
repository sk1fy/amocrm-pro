package widgetauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL implementation of Repository.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// FindVerificationMaterial intentionally uses only unverified client_uuid and
// account_id hints. None of the returned or hinted values authorize a request;
// Authenticate verifies the signature and all claims afterwards.
func (s *Store) FindVerificationMaterial(
	ctx context.Context,
	clientUUID string,
	accountID int64,
) (VerificationMaterial, error) {
	var material VerificationMaterial
	err := s.pool.QueryRow(ctx, `
		SELECT integration.id,
			installation.id,
			integration.client_id,
			integration.client_secret_ciphertext,
			integration.client_secret_key_version,
			integration.redirect_uri,
			installation.account_id,
			installation.account_domain
		FROM integrations AS integration
		JOIN installations AS installation
			ON installation.integration_id = integration.id
		WHERE integration.client_id = $1
		  AND installation.account_id = $2
		  AND integration.status = 'active'
		  AND installation.status = 'active'`,
		clientUUID,
		accountID,
	).Scan(
		&material.IntegrationID,
		&material.InstallationID,
		&material.ClientUUID,
		&material.ClientSecretCiphertext,
		&material.ClientSecretKeyVersion,
		&material.RedirectURI,
		&material.AccountID,
		&material.AccountDomain,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerificationMaterial{}, ErrUnknownTenant
	}
	if err != nil {
		return VerificationMaterial{}, fmt.Errorf("find widget verification material: %w", err)
	}
	return material, nil
}

// ConsumeToken inserts a jti exactly once. ON CONFLICT makes replay detection
// atomic across all API replicas without Redis.
func (s *Store) ConsumeToken(ctx context.Context, token UsedToken) error {
	command, err := s.pool.Exec(ctx, `
		INSERT INTO used_widget_tokens (
			integration_id, jti, issuer, account_id, user_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (integration_id, jti) DO NOTHING`,
		token.IntegrationID,
		token.TokenID,
		token.Issuer,
		token.AccountID,
		token.UserID,
		token.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("consume widget token: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrReplay
	}
	return nil
}
