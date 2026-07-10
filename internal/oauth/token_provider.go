package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type TokenProvider struct {
	pool         *pgxpool.Pool
	cipher       Cipher
	gateway      OAuthGateway
	refreshAhead time.Duration
}

func NewTokenProvider(pool *pgxpool.Pool, cipher Cipher, gateway OAuthGateway) *TokenProvider {
	return &TokenProvider{
		pool: pool, cipher: cipher, gateway: gateway, refreshAhead: time.Minute,
	}
}

func (p *TokenProvider) Token(ctx context.Context, installationID uuid.UUID, force bool) (amocrm.AccessToken, error) {
	snapshot, err := loadCredential(ctx, p.pool, installationID, false)
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if !force && snapshot.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		return p.decryptAccess(snapshot)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("begin token refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := loadCredential(ctx, tx, installationID, true)
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if locked.TokenVersion != snapshot.TokenVersion && locked.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		if err := tx.Commit(ctx); err != nil {
			return amocrm.AccessToken{}, fmt.Errorf("commit observed token refresh: %w", err)
		}
		return p.decryptAccess(locked)
	}
	if !force && locked.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		if err := tx.Commit(ctx); err != nil {
			return amocrm.AccessToken{}, fmt.Errorf("commit fresh token read: %w", err)
		}
		return p.decryptAccess(locked)
	}

	refreshToken, err := p.cipher.Open(locked.KeyVersion, locked.RefreshTokenCiphertext, credentialsAAD(installationID))
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("decrypt refresh token: %w", err)
	}
	clientSecret, err := p.cipher.Open(
		locked.ClientSecretKeyVersion,
		locked.ClientSecretCiphertext,
		integrationSecretAAD(locked.IntegrationID),
	)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("decrypt integration secret: %w", err)
	}

	refreshed, err := p.gateway.Refresh(
		ctx,
		locked.AccountDomain,
		locked.ClientID,
		string(clientSecret),
		locked.RedirectURI,
		string(refreshToken),
	)
	if err != nil {
		var apiError *amocrm.APIError
		if errors.As(err, &apiError) && (apiError.Kind == amocrm.ErrorUnauthorized || apiError.Kind == amocrm.ErrorValidation) {
			if _, updateErr := tx.Exec(ctx, `
				UPDATE installations SET status = 'reauth_required', updated_at = now() WHERE id = $1`, installationID,
			); updateErr != nil {
				return amocrm.AccessToken{}, fmt.Errorf("mark installation reauthorization required: %w", updateErr)
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return amocrm.AccessToken{}, fmt.Errorf("commit reauthorization state: %w", commitErr)
			}
		}
		return amocrm.AccessToken{}, fmt.Errorf("refresh amoCRM token: %w", err)
	}

	accessCiphertext, keyVersion, err := p.cipher.Seal([]byte(refreshed.AccessToken), credentialsAAD(installationID))
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("encrypt refreshed access token: %w", err)
	}
	refreshCiphertext, refreshKeyVersion, err := p.cipher.Seal([]byte(refreshed.RefreshToken), credentialsAAD(installationID))
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("encrypt refreshed refresh token: %w", err)
	}
	if refreshKeyVersion != keyVersion {
		return amocrm.AccessToken{}, errors.New("active encryption key changed during token refresh")
	}
	expiresAt := tokenExpiry(refreshed)
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_credentials
		SET access_token_ciphertext = $2, refresh_token_ciphertext = $3,
			expires_at = $4, token_version = token_version + 1,
			key_version = $5, refreshed_at = now(), updated_at = now()
		WHERE installation_id = $1`,
		installationID, accessCiphertext, refreshCiphertext, expiresAt, keyVersion,
	); err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("save refreshed token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("commit token refresh: %w", err)
	}

	locked.AccessTokenCiphertext = accessCiphertext
	locked.RefreshTokenCiphertext = refreshCiphertext
	locked.ExpiresAt = expiresAt
	locked.KeyVersion = keyVersion
	locked.TokenVersion++
	return p.decryptAccess(locked)
}

func (p *TokenProvider) MarkReauthRequired(ctx context.Context, installationID uuid.UUID) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE installations
		SET status = 'reauth_required', updated_at = now()
		WHERE id = $1 AND status <> 'uninstalled'`, installationID,
	)
	if err != nil {
		return fmt.Errorf("mark installation reauthorization required: %w", err)
	}
	return nil
}

func (p *TokenProvider) decryptAccess(credential credential) (amocrm.AccessToken, error) {
	accessToken, err := p.cipher.Open(
		credential.KeyVersion,
		credential.AccessTokenCiphertext,
		credentialsAAD(credential.InstallationID),
	)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("decrypt access token: %w", err)
	}
	return amocrm.AccessToken{
		InstallationID: credential.InstallationID,
		IntegrationID:  credential.IntegrationID,
		AccountID:      credential.AccountID,
		AccountDomain:  credential.AccountDomain,
		Value:          string(accessToken),
	}, nil
}

type credential struct {
	InstallationID         uuid.UUID
	IntegrationID          uuid.UUID
	AccountID              int64
	AccountDomain          string
	ClientID               string
	ClientSecretCiphertext []byte
	ClientSecretKeyVersion int
	RedirectURI            string
	AccessTokenCiphertext  []byte
	RefreshTokenCiphertext []byte
	ExpiresAt              time.Time
	TokenVersion           int64
	KeyVersion             int
}

type credentialQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadCredential(ctx context.Context, query credentialQuerier, installationID uuid.UUID, lock bool) (credential, error) {
	lockClause := ""
	if lock {
		lockClause = " FOR UPDATE OF credentials"
	}
	var value credential
	err := query.QueryRow(ctx, `
		SELECT installation.id, installation.integration_id, installation.account_id,
			installation.account_domain, integration.client_id,
			integration.client_secret_ciphertext, integration.client_secret_key_version,
			integration.redirect_uri, credentials.access_token_ciphertext,
			credentials.refresh_token_ciphertext, credentials.expires_at,
			credentials.token_version, credentials.key_version
		FROM oauth_credentials credentials
		JOIN installations installation ON installation.id = credentials.installation_id
		JOIN integrations integration ON integration.id = installation.integration_id
		WHERE credentials.installation_id = $1
		  AND installation.status IN ('active', 'authorizing')
		  AND integration.status = 'active'`+lockClause,
		installationID,
	).Scan(
		&value.InstallationID,
		&value.IntegrationID,
		&value.AccountID,
		&value.AccountDomain,
		&value.ClientID,
		&value.ClientSecretCiphertext,
		&value.ClientSecretKeyVersion,
		&value.RedirectURI,
		&value.AccessTokenCiphertext,
		&value.RefreshTokenCiphertext,
		&value.ExpiresAt,
		&value.TokenVersion,
		&value.KeyVersion,
	)
	if err != nil {
		return credential{}, fmt.Errorf("load OAuth credential: %w", err)
	}
	return value, nil
}
