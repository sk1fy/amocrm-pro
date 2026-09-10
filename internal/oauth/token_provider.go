package oauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	uninstallID  uuid.UUID
	leaseTTL     time.Duration
	leasePoll    time.Duration
	mu           sync.Mutex
	pending      map[uuid.UUID]pendingRotation
}

// pendingRotation keeps tokens after amoCRM accepted a one-time refresh and
// local persist has not yet committed. The consumed refresh token must not be
// sent again; a crash before persist requires reauthorization.
type pendingRotation struct {
	observedVersion int64
	leaseToken      uuid.UUID
	token           Token
}

const (
	tokenFinalizeTimeout = 5 * time.Second
	tokenRollbackTimeout = 2 * time.Second
	refreshLeaseTTL      = 45 * time.Second
	refreshLeasePoll     = 25 * time.Millisecond
	tokenPersistAttempts = 5
)

func NewTokenProvider(pool *pgxpool.Pool, cipher Cipher, gateway OAuthGateway) *TokenProvider {
	return &TokenProvider{
		pool: pool, cipher: cipher, gateway: gateway, refreshAhead: time.Minute,
		pending: make(map[uuid.UUID]pendingRotation),
	}
}

// ErrRefreshOutcomeUnknown means an earlier remote refresh may have consumed
// the token. Only that claim's result or reauthorization may resolve it.
var ErrRefreshOutcomeUnknown = errors.New("OAuth refresh outcome unknown; recover the pending result or reauthorize")
var errRefreshFenceLost = errors.New("OAuth refresh claim was superseded")

// NewUninstallTokenProvider is an operator-only capability bound to one already
// uninstalled installation. It shares the normal refresh/fencing implementation
// without reopening product access or changing installation status.
func NewUninstallTokenProvider(pool *pgxpool.Pool, cipher Cipher, gateway OAuthGateway, installationID uuid.UUID) (*TokenProvider, error) {
	if installationID == uuid.Nil {
		return nil, errors.New("uninstall installation id is required")
	}
	p := NewTokenProvider(pool, cipher, gateway)
	p.uninstallID = installationID
	return p, nil
}

func (p *TokenProvider) load(ctx context.Context, q credentialQuerier, id uuid.UUID, lock bool) (credential, error) {
	if p.uninstallID == uuid.Nil {
		return loadCredential(ctx, q, id, lock)
	}
	if id != p.uninstallID {
		return credential{}, errors.New("uninstall credential scope mismatch")
	}
	return loadCredentialFiltered(ctx, q, id, lock, ` AND installation.status='uninstalled'`)
}

func (p *TokenProvider) Token(ctx context.Context, installationID uuid.UUID) (amocrm.AccessToken, error) {
	snapshot, err := p.load(ctx, p.pool, installationID, false)
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
	for {
		if p.uninstallID != uuid.Nil {
			if _, err := p.load(ctx, p.pool, installationID, false); err != nil {
				return amocrm.AccessToken{}, err
			}
		}
		if token, ok := p.pendingToken(installationID, observedVersion); ok {
			return p.persistRotated(ctx, installationID, token)
		}
		claim, access, wait, err := p.captureRefresh(ctx, installationID, observedVersion, forced)
		if err != nil {
			return amocrm.AccessToken{}, err
		}
		if !wait {
			if claim == nil {
				return access, nil
			}
			return p.rotateClaimed(ctx, *claim)
		}
		timer := time.NewTimer(p.pollInterval())
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return amocrm.AccessToken{}, fmt.Errorf("wait for OAuth refresh lease: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

type refreshClaim struct {
	credential credential
	leaseToken uuid.UUID
}

func (p *TokenProvider) captureRefresh(
	ctx context.Context,
	installationID uuid.UUID,
	observedVersion int64,
	forced bool,
) (*refreshClaim, amocrm.AccessToken, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, amocrm.AccessToken{}, false, fmt.Errorf("begin token refresh: %w", err)
	}
	defer func() { _ = rollbackTokenTransaction(tx) }()

	locked, err := p.load(ctx, tx, installationID, true)
	if err != nil {
		return nil, amocrm.AccessToken{}, false, err
	}
	if locked.TokenVersion != observedVersion {
		p.forgetPending(installationID, observedVersion)
		access, decryptErr := p.decryptAccess(locked)
		if decryptErr != nil {
			return nil, amocrm.AccessToken{}, false, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, amocrm.AccessToken{}, false, fmt.Errorf("commit observed token refresh: %w", err)
		}
		return nil, access, false, nil
	}
	if !forced && locked.ExpiresAt.After(time.Now().Add(p.refreshAhead)) {
		access, decryptErr := p.decryptAccess(locked)
		if decryptErr != nil {
			return nil, amocrm.AccessToken{}, false, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, amocrm.AccessToken{}, false, fmt.Errorf("commit fresh token read: %w", err)
		}
		return nil, access, false, nil
	}
	if locked.LeaseHeld {
		if err := tx.Commit(ctx); err != nil {
			return nil, amocrm.AccessToken{}, false, fmt.Errorf("commit contended token refresh: %w", err)
		}
		return nil, amocrm.AccessToken{}, true, nil
	}

	if locked.LeaseToken != nil {
		// An expired claim is an unknown remote outcome, not a reusable token.
		return nil, amocrm.AccessToken{}, false, ErrRefreshOutcomeUnknown
	}

	leaseToken := uuid.New()
	tag, err := tx.Exec(ctx, `
		UPDATE oauth_credentials
		SET lease_token = $2, lease_until = now() + $3 * interval '1 millisecond', updated_at = now()
		WHERE installation_id = $1
		  AND token_version = $4
		  AND lease_token IS NULL`,
		installationID, leaseToken, p.leaseDuration().Milliseconds(), observedVersion,
	)
	if err != nil {
		return nil, amocrm.AccessToken{}, false, fmt.Errorf("claim token refresh lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		if err := tx.Commit(ctx); err != nil {
			return nil, amocrm.AccessToken{}, false, fmt.Errorf("commit unclaimed token refresh: %w", err)
		}
		return nil, amocrm.AccessToken{}, true, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, amocrm.AccessToken{}, false, fmt.Errorf("commit token refresh lease: %w", err)
	}
	return &refreshClaim{credential: locked, leaseToken: leaseToken}, amocrm.AccessToken{}, false, nil
}

func (p *TokenProvider) rotateClaimed(ctx context.Context, claim refreshClaim) (amocrm.AccessToken, error) {
	installationID := claim.credential.InstallationID
	observedVersion := claim.credential.TokenVersion
	if token, ok := p.pendingToken(installationID, observedVersion); ok {
		return p.persistRotated(ctx, installationID, token)
	}

	refreshToken, err := p.cipher.Open(claim.credential.KeyVersion, claim.credential.RefreshTokenCiphertext, credentialsAAD(installationID))
	if err != nil {
		if releaseErr := p.releaseLease(installationID, claim.leaseToken, observedVersion); releaseErr != nil {
			return amocrm.AccessToken{}, errors.Join(
				fmt.Errorf("decrypt refresh token: %w", err),
				releaseErr,
			)
		}
		return amocrm.AccessToken{}, fmt.Errorf("decrypt refresh token: %w", err)
	}
	defer clear(refreshToken)
	clientSecret, err := p.cipher.Open(
		claim.credential.ClientSecretKeyVersion,
		claim.credential.ClientSecretCiphertext,
		integrationSecretAAD(claim.credential.IntegrationID),
	)
	if err != nil {
		if releaseErr := p.releaseLease(installationID, claim.leaseToken, observedVersion); releaseErr != nil {
			return amocrm.AccessToken{}, errors.Join(
				fmt.Errorf("decrypt integration secret: %w", err),
				releaseErr,
			)
		}
		return amocrm.AccessToken{}, fmt.Errorf("decrypt integration secret: %w", err)
	}
	defer clear(clientSecret)
	if err := ctx.Err(); err != nil {
		return amocrm.AccessToken{}, errors.Join(err, p.releaseLease(installationID, claim.leaseToken, observedVersion))
	}

	refreshed, err := p.gateway.Refresh(
		ctx,
		claim.credential.AccountDomain,
		claim.credential.ClientID,
		string(clientSecret),
		claim.credential.RedirectURI,
		string(refreshToken),
	)
	if err != nil {
		var apiError *amocrm.APIError
		if errors.As(err, &apiError) && (apiError.Kind == amocrm.ErrorUnauthorized || apiError.Kind == amocrm.ErrorValidation) {
			if markErr := p.rejectClaim(installationID, claim.leaseToken, observedVersion); markErr != nil {
				return amocrm.AccessToken{}, errors.Join(err, markErr)
			}
		} else if errors.As(err, &apiError) && apiError.Kind == amocrm.ErrorRateLimited {
			if releaseErr := p.releaseLease(installationID, claim.leaseToken, observedVersion); releaseErr != nil {
				return amocrm.AccessToken{}, errors.Join(err, releaseErr)
			}
		}
		// Network errors, cancellation and 5xx cannot prove the one-time token
		// was not consumed. Keep the durable claim; never repeat that refresh.
		return amocrm.AccessToken{}, fmt.Errorf("refresh amoCRM token: %w", err)
	}

	p.rememberPending(installationID, observedVersion, claim.leaseToken, refreshed)
	return p.persistRotated(ctx, installationID, pendingRotation{observedVersion: observedVersion, leaseToken: claim.leaseToken, token: refreshed})
}

func (p *TokenProvider) persistRotated(
	ctx context.Context,
	installationID uuid.UUID,
	rotation pendingRotation,
) (amocrm.AccessToken, error) {
	observedVersion := rotation.observedVersion
	var lastErr error
	for range tokenPersistAttempts {
		finalizeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenFinalizeTimeout)
		access, err := p.finalizeRotated(finalizeContext, installationID, rotation)
		cancel()
		if err == nil {
			p.forgetPending(installationID, observedVersion)
			return access, nil
		}
		if errors.Is(err, errRefreshFenceLost) {
			p.forgetPending(installationID, observedVersion)
			return amocrm.AccessToken{}, err
		}
		lastErr = err
	}
	return amocrm.AccessToken{}, fmt.Errorf("save refreshed token: %w", lastErr)
}

func (p *TokenProvider) finalizeRotated(
	ctx context.Context,
	installationID uuid.UUID,
	rotation pendingRotation,
) (amocrm.AccessToken, error) {
	observedVersion, refreshed := rotation.observedVersion, rotation.token
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

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return amocrm.AccessToken{}, fmt.Errorf("begin token refresh persist: %w", err)
	}
	defer func() { _ = rollbackTokenTransaction(tx) }()

	locked, err := loadCredentialRow(ctx, tx, installationID, true, false)
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if locked.TokenVersion != observedVersion {
		p.forgetPending(installationID, observedVersion)
		access, decryptErr := p.decryptAccess(locked)
		if decryptErr != nil {
			return amocrm.AccessToken{}, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return amocrm.AccessToken{}, fmt.Errorf("commit observed token refresh: %w", err)
		}
		return access, nil
	}
	if locked.LeaseToken == nil || *locked.LeaseToken != rotation.leaseToken {
		return amocrm.AccessToken{}, errRefreshFenceLost
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_credentials
		SET access_token_ciphertext = $2, refresh_token_ciphertext = $3,
			expires_at = $4, token_version = token_version + 1,
			key_version = $5, refreshed_at = now(), updated_at = now(),
			lease_token = NULL, lease_until = NULL
		WHERE installation_id = $1 AND token_version = $6 AND lease_token = $7`,
		installationID, accessCiphertext, refreshCiphertext, expiresAt, keyVersion, observedVersion, rotation.leaseToken,
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
	locked.LeaseHeld = false
	return p.decryptAccess(locked)
}

func (p *TokenProvider) releaseLease(
	installationID uuid.UUID,
	leaseToken uuid.UUID,
	observedVersion int64,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenFinalizeTimeout)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
		UPDATE oauth_credentials
		SET lease_token = NULL, lease_until = NULL, updated_at = now()
		WHERE installation_id = $1 AND lease_token = $2 AND token_version = $3`,
		installationID, leaseToken, observedVersion,
	); err != nil {
		return fmt.Errorf("release failed token refresh lease: %w", err)
	}
	return nil
}

func (p *TokenProvider) MarkReauthRequired(
	ctx context.Context,
	installationID uuid.UUID,
	observedVersion int64,
) error {
	if p.uninstallID != uuid.Nil && p.uninstallID != installationID {
		return errors.New("uninstall credential scope mismatch")
	}
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
	var leaseHeld bool
	err = tx.QueryRow(ctx, `
		SELECT token_version,
			lease_token IS NOT NULL
		FROM oauth_credentials WHERE installation_id=$1 FOR UPDATE`, installationID,
	).Scan(&currentVersion, &leaseHeld)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current OAuth token version: %w", err)
	}
	if currentVersion != observedVersion || leaseHeld {
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

// Complete a definitive invalid_grant/401 under the same lock order as
// reauthorization. Clearing the lease and marking auth failure must be atomic.
func (p *TokenProvider) rejectClaim(id, lease uuid.UUID, version int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenFinalizeTimeout)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rollbackTokenTransaction(tx) }()
	var state string
	if err := tx.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1 FOR UPDATE`, id).Scan(&state); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE oauth_credentials SET lease_token=NULL,lease_until=NULL,updated_at=now() WHERE installation_id=$1 AND token_version=$2 AND lease_token=$3`, id, version, lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 && state != "disabled" && state != "uninstalled" {
		if _, err := tx.Exec(ctx, `UPDATE installations SET status='reauth_required',updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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

func (p *TokenProvider) leaseDuration() time.Duration {
	if p.leaseTTL > 0 {
		return p.leaseTTL
	}
	return refreshLeaseTTL
}

func (p *TokenProvider) pollInterval() time.Duration {
	if p.leasePoll > 0 {
		return p.leasePoll
	}
	return refreshLeasePoll
}

func (p *TokenProvider) rememberPending(installationID uuid.UUID, observedVersion int64, leaseToken uuid.UUID, token Token) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[uuid.UUID]pendingRotation)
	}
	if current, ok := p.pending[installationID]; ok && current.observedVersion > observedVersion {
		return
	}
	p.pending[installationID] = pendingRotation{observedVersion: observedVersion, leaseToken: leaseToken, token: token}
}

func (p *TokenProvider) pendingToken(installationID uuid.UUID, observedVersion int64) (pendingRotation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, ok := p.pending[installationID]
	if !ok {
		return pendingRotation{}, false
	}
	if item.observedVersion != observedVersion {
		return pendingRotation{}, false
	}
	return item, true
}

func (p *TokenProvider) forgetPending(installationID uuid.UUID, observedVersion int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current, ok := p.pending[installationID]; ok && current.observedVersion == observedVersion {
		delete(p.pending, installationID)
	}
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
	LeaseHeld              bool
	LeaseToken             *uuid.UUID
}

type credentialQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadCredential(ctx context.Context, query credentialQuerier, installationID uuid.UUID, lock bool) (credential, error) {
	return loadCredentialRow(ctx, query, installationID, lock, true)
}

func loadCredentialRow(ctx context.Context, query credentialQuerier, installationID uuid.UUID, lock, requireActive bool) (credential, error) {
	statusFilter := ""
	if requireActive {
		statusFilter = ` AND installation.status IN ('active', 'authorizing') AND integration.status='active'`
	}
	return loadCredentialFiltered(ctx, query, installationID, lock, statusFilter)
}

func loadCredentialFiltered(ctx context.Context, query credentialQuerier, installationID uuid.UUID, lock bool, statusFilter string) (credential, error) {
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
			credentials.token_version, credentials.key_version,
			credentials.lease_token IS NOT NULL AND credentials.lease_until IS NOT NULL
				AND credentials.lease_until > now(), credentials.lease_token
		FROM oauth_credentials credentials
		JOIN installations installation ON installation.id = credentials.installation_id
		JOIN integrations integration ON integration.id = installation.integration_id
		WHERE credentials.installation_id = $1`+statusFilter+lockClause,
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
		&value.LeaseHeld,
		&value.LeaseToken,
	)
	if err != nil {
		return credential{}, fmt.Errorf("load OAuth credential: %w", err)
	}
	return value, nil
}
