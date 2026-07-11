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

const (
	tokenFinalizeTimeout = 5 * time.Second
	tokenRollbackTimeout = 2 * time.Second
	reauthUpdateTimeout  = 2 * time.Second
)

func NewTokenProvider(pool *pgxpool.Pool, cipher Cipher, gateway OAuthGateway) *TokenProvider {
	return &TokenProvider{
		pool: pool, cipher: cipher, gateway: gateway, refreshAhead: time.Minute,
	}
}

func (p *TokenProvider) Token(ctx context.Context, installationID uuid.UUID) (amocrm.AccessToken, error) {
	snapshot, err := loadCredential(ctx, p.pool, installationID, false)
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if snapshot.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		return p.decryptAccess(snapshot)
	}
	return p.refreshIfVersion(ctx, installationID, snapshot.TokenVersion, false)
}

func (p *TokenProvider) RefreshIfCurrent(
	ctx context.Context,
	observed amocrm.AccessToken,
) (amocrm.AccessToken, error) {
	if observed.InstallationID == uuid.Nil || observed.TokenVersion <= 0 {
		return amocrm.AccessToken{}, errors.New("observed OAuth token identity and version are required")
	}
	return p.refreshIfVersion(ctx, observed.InstallationID, observed.TokenVersion, true)
}

func (p *TokenProvider) refreshIfVersion(
	ctx context.Context,
	installationID uuid.UUID,
	observedVersion int64,
	forced bool,
) (amocrm.AccessToken, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("begin token refresh: %w", err)
	}
	defer func() { _ = rollbackTokenTransaction(tx) }()

	locked, err := loadCredential(ctx, tx, installationID, true)
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if locked.TokenVersion != observedVersion {
		access, decryptErr := p.decryptAccess(locked)
		if decryptErr != nil {
			return amocrm.AccessToken{}, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return amocrm.AccessToken{}, fmt.Errorf("commit observed token refresh: %w", err)
		}
		return access, nil
	}
	if !forced && locked.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		access, decryptErr := p.decryptAccess(locked)
		if decryptErr != nil {
			return amocrm.AccessToken{}, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return amocrm.AccessToken{}, fmt.Errorf("commit fresh token read: %w", err)
		}
		return access, nil
	}

	refreshToken, err := p.cipher.Open(locked.KeyVersion, locked.RefreshTokenCiphertext, credentialsAAD(installationID))
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("decrypt refresh token: %w", err)
	}
	defer clear(refreshToken)
	clientSecret, err := p.cipher.Open(
		locked.ClientSecretKeyVersion,
		locked.ClientSecretCiphertext,
		integrationSecretAAD(locked.IntegrationID),
	)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("decrypt integration secret: %w", err)
	}
	defer clear(clientSecret)

	refreshed, err := p.gateway.Refresh(
		ctx,
		locked.AccountDomain,
		locked.ClientID,
		string(clientSecret),
		locked.RedirectURI,
		string(refreshToken),
	)
	if err != nil {
		if rollbackErr := rollbackTokenTransaction(tx); rollbackErr != nil {
			return amocrm.AccessToken{}, errors.Join(
				fmt.Errorf("refresh amoCRM token: %w", err),
				fmt.Errorf("rollback failed token refresh: %w", rollbackErr),
			)
		}
		var apiError *amocrm.APIError
		if errors.As(err, &apiError) && (apiError.Kind == amocrm.ErrorUnauthorized || apiError.Kind == amocrm.ErrorValidation) {
			markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), reauthUpdateTimeout)
			markErr := p.MarkReauthRequired(markContext, installationID, locked.TokenVersion)
			cancel()
			if markErr != nil {
				return amocrm.AccessToken{}, errors.Join(
					fmt.Errorf("refresh amoCRM token: %w", err),
					fmt.Errorf("mark installation reauthorization required: %w", markErr),
				)
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
	finalizeContext, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), tokenFinalizeTimeout)
	defer cancelFinalize()
	if _, err := tx.Exec(finalizeContext, `
		UPDATE oauth_credentials
		SET access_token_ciphertext = $2, refresh_token_ciphertext = $3,
			expires_at = $4, token_version = token_version + 1,
			key_version = $5, refreshed_at = now(), updated_at = now()
		WHERE installation_id = $1`,
		installationID, accessCiphertext, refreshCiphertext, expiresAt, keyVersion,
	); err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("save refreshed token: %w", err)
	}
	if err := tx.Commit(finalizeContext); err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("commit token refresh: %w", err)
	}

	locked.AccessTokenCiphertext = accessCiphertext
	locked.RefreshTokenCiphertext = refreshCiphertext
	locked.ExpiresAt = expiresAt
	locked.KeyVersion = keyVersion
	locked.TokenVersion++
	return p.decryptAccess(locked)
}

func (p *TokenProvider) MarkReauthRequired(
	ctx context.Context,
	installationID uuid.UUID,
	observedVersion int64,
) error {
	if observedVersion <= 0 {
		return errors.New("observed OAuth token version is required")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reauthorization state update: %w", err)
	}
	defer func() { _ = rollbackTokenTransaction(tx) }()

	var installationStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM installations WHERE id=$1 FOR UPDATE`, installationID,
	).Scan(&installationStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock installation for reauthorization state: %w", err)
	}
	if installationStatus == "disabled" || installationStatus == "uninstalled" {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit skipped reauthorization state: %w", err)
		}
		return nil
	}

	var currentVersion int64
	err = tx.QueryRow(ctx, `
		SELECT token_version FROM oauth_credentials WHERE installation_id=$1 FOR UPDATE`, installationID,
	).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current OAuth token version: %w", err)
	}
	if currentVersion != observedVersion {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit stale reauthorization state check: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE installations SET status='reauth_required', updated_at=now() WHERE id=$1`, installationID,
	); err != nil {
		return fmt.Errorf("mark installation reauthorization required: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reauthorization state: %w", err)
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
		TokenVersion:   credential.TokenVersion,
	}, nil
}

func rollbackTokenTransaction(tx pgx.Tx) error {
	rollbackContext, cancel := context.WithTimeout(context.Background(), tokenRollbackTimeout)
	defer cancel()
	err := tx.Rollback(rollbackContext)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
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
