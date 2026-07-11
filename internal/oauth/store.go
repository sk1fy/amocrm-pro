package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrIntegrationNotFound = errors.New("integration not found")
	ErrInvalidState        = errors.New("OAuth state is invalid, expired, or already used")
)

var integrationCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}[a-z0-9]$`)

type IntegrationInput struct {
	Code          string
	ClientID      string
	ClientSecret  string
	RedirectURI   string
	WebhookEvents []string
}

type Store struct {
	pool   *pgxpool.Pool
	cipher Cipher
}

func NewStore(pool *pgxpool.Pool, cipher Cipher) *Store {
	return &Store{pool: pool, cipher: cipher}
}

func (s *Store) EnsureIntegration(ctx context.Context, input IntegrationInput) (Integration, error) {
	if !integrationCodePattern.MatchString(input.Code) {
		return Integration{}, errors.New("integration code must contain 3-64 lowercase letters, digits, underscores, or hyphens")
	}
	if _, err := uuid.Parse(input.ClientID); err != nil {
		return Integration{}, errors.New("amoCRM client id must be a UUID")
	}
	redirect, err := url.Parse(input.RedirectURI)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.User != nil || redirect.Fragment != "" {
		return Integration{}, errors.New("amoCRM redirect URI must be an absolute HTTPS URL without user info or fragment")
	}
	if input.ClientSecret == "" {
		return Integration{}, errors.New("amoCRM client secret is required")
	}
	for _, event := range input.WebhookEvents {
		if strings.TrimSpace(event) == "" || len(event) > 100 {
			return Integration{}, errors.New("webhook event names must be non-empty and at most 100 bytes")
		}
	}
	events, err := json.Marshal(input.WebhookEvents)
	if err != nil {
		return Integration{}, fmt.Errorf("encode webhook events: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Integration{}, fmt.Errorf("begin integration bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "integration:"+input.Code); err != nil {
		return Integration{}, fmt.Errorf("lock integration bootstrap: %w", err)
	}
	integrationID := uuid.New()
	err = tx.QueryRow(ctx, `SELECT id FROM integrations WHERE code = $1 FOR UPDATE`, input.Code).Scan(&integrationID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Integration{}, fmt.Errorf("find bootstrap integration: %w", err)
	}
	secret := []byte(input.ClientSecret)
	ciphertext, keyVersion, err := s.cipher.Seal(secret, integrationSecretAAD(integrationID))
	clear(secret)
	if err != nil {
		return Integration{}, fmt.Errorf("encrypt integration secret: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext,
			client_secret_key_version, redirect_uri, status, webhook_events
		) VALUES ($1, $2, $3, $4, $5, $6, 'active', $7)
		ON CONFLICT (code) DO UPDATE
		SET client_id = EXCLUDED.client_id,
			client_secret_ciphertext = EXCLUDED.client_secret_ciphertext,
			client_secret_key_version = EXCLUDED.client_secret_key_version,
			redirect_uri = EXCLUDED.redirect_uri,
			status = 'active', webhook_events = EXCLUDED.webhook_events,
			updated_at = now()`,
		integrationID, input.Code, input.ClientID, ciphertext, keyVersion, input.RedirectURI, events,
	); err != nil {
		return Integration{}, fmt.Errorf("upsert bootstrap integration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Integration{}, fmt.Errorf("commit integration bootstrap: %w", err)
	}
	return s.FindIntegrationByCode(ctx, input.Code)
}

func (s *Store) FindIntegrationByCode(ctx context.Context, code string) (Integration, error) {
	var integration Integration
	var events json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT id, code, client_id, client_secret_ciphertext,
			client_secret_key_version, redirect_uri, webhook_events
		FROM integrations
		WHERE code = $1 AND status = 'active'`, code,
	).Scan(
		&integration.ID,
		&integration.Code,
		&integration.ClientID,
		&integration.ClientSecretCiphertext,
		&integration.ClientSecretKeyVersion,
		&integration.RedirectURI,
		&events,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Integration{}, ErrIntegrationNotFound
	}
	if err != nil {
		return Integration{}, fmt.Errorf("find integration: %w", err)
	}
	if err := json.Unmarshal(events, &integration.WebhookEvents); err != nil {
		return Integration{}, fmt.Errorf("decode integration webhook events: %w", err)
	}
	return integration, nil
}

func (s *Store) CreateState(
	ctx context.Context,
	integrationID uuid.UUID,
	returnURL string,
	ttl time.Duration,
) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(state))
	expiresAt := time.Now().UTC().Add(ttl)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_states (integration_id, state_hash, return_url, expires_at)
		VALUES ($1, $2, $3, $4)`, integrationID, hash[:], returnURL, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("save OAuth state: %w", err)
	}
	return state, expiresAt, nil
}

func (s *Store) ConsumeState(ctx context.Context, rawState string) (State, error) {
	hash := sha256.Sum256([]byte(rawState))
	var state State
	var events json.RawMessage
	err := s.pool.QueryRow(ctx, `
		WITH consumed AS (
			UPDATE oauth_states
			SET consumed_at = now()
			WHERE state_hash = $1 AND consumed_at IS NULL AND expires_at > now()
			RETURNING id, integration_id, return_url, expires_at
		)
		SELECT consumed.id, COALESCE(consumed.return_url, ''), consumed.expires_at,
			i.id, i.code, i.client_id, i.client_secret_ciphertext,
			i.client_secret_key_version, i.redirect_uri, i.webhook_events
		FROM consumed
		JOIN integrations i ON i.id = consumed.integration_id
		WHERE i.status = 'active'`, hash[:],
	).Scan(
		&state.ID,
		&state.ReturnURL,
		&state.ExpiresAt,
		&state.Integration.ID,
		&state.Integration.Code,
		&state.Integration.ClientID,
		&state.Integration.ClientSecretCiphertext,
		&state.Integration.ClientSecretKeyVersion,
		&state.Integration.RedirectURI,
		&events,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrInvalidState
	}
	if err != nil {
		return State{}, fmt.Errorf("consume OAuth state: %w", err)
	}
	if err := json.Unmarshal(events, &state.Integration.WebhookEvents); err != nil {
		return State{}, fmt.Errorf("decode integration webhook events: %w", err)
	}
	return state, nil
}

func (s *Store) SaveInstallation(
	ctx context.Context,
	integration Integration,
	account Account,
	accountDomain string,
	token Token,
) (InstallationResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InstallationResult{}, fmt.Errorf("begin save installation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidateID := uuid.New()
	var installationID uuid.UUID
	var existingWebhookHash, existingWebhookCiphertext []byte
	var existingWebhookKeyVersion *int
	if err := tx.QueryRow(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status,
			webhook_status, webhook_settings
		) VALUES ($1, $2, $3, $4, 'active', 'pending', $5)
		ON CONFLICT (integration_id, account_id) DO UPDATE
		SET account_domain = EXCLUDED.account_domain,
			status = 'active',
			webhook_status = 'pending',
			webhook_settings = EXCLUDED.webhook_settings,
			webhook_last_error = NULL,
			updated_at = now()
		RETURNING id, webhook_key_hash, webhook_key_ciphertext, webhook_key_key_version`,
		candidateID, integration.ID, account.ID, accountDomain, integration.WebhookEvents,
	).Scan(&installationID, &existingWebhookHash, &existingWebhookCiphertext, &existingWebhookKeyVersion); err != nil {
		return InstallationResult{}, fmt.Errorf("upsert installation: %w", err)
	}

	if len(existingWebhookHash) == 0 || len(existingWebhookCiphertext) == 0 || existingWebhookKeyVersion == nil {
		plainWebhookKey := make([]byte, 32)
		if _, err := rand.Read(plainWebhookKey); err != nil {
			return InstallationResult{}, fmt.Errorf("generate webhook key: %w", err)
		}
		encodedWebhookKey := base64.RawURLEncoding.EncodeToString(plainWebhookKey)
		webhookHash := sha256.Sum256([]byte(encodedWebhookKey))
		webhookCiphertext, keyVersion, err := s.cipher.Seal([]byte(encodedWebhookKey), webhookKeyAAD(installationID))
		if err != nil {
			return InstallationResult{}, fmt.Errorf("encrypt webhook key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE installations
			SET webhook_key_hash = $2, webhook_key_ciphertext = $3,
				webhook_key_key_version = $4, webhook_status = 'pending', updated_at = now()
			WHERE id = $1`, installationID, webhookHash[:], webhookCiphertext, keyVersion); err != nil {
			return InstallationResult{}, fmt.Errorf("save webhook key: %w", err)
		}
	}

	accessCiphertext, keyVersion, err := s.cipher.Seal([]byte(token.AccessToken), credentialsAAD(installationID))
	if err != nil {
		return InstallationResult{}, fmt.Errorf("encrypt access token: %w", err)
	}
	refreshCiphertext, refreshKeyVersion, err := s.cipher.Seal([]byte(token.RefreshToken), credentialsAAD(installationID))
	if err != nil {
		return InstallationResult{}, fmt.Errorf("encrypt refresh token: %w", err)
	}
	if refreshKeyVersion != keyVersion {
		return InstallationResult{}, errors.New("active encryption key changed during credential encryption")
	}
	expiresAt := tokenExpiry(token)
	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_credentials (
			installation_id, access_token_ciphertext, refresh_token_ciphertext,
			expires_at, token_version, key_version, refreshed_at
		) VALUES ($1, $2, $3, $4, 1, $5, now())
		ON CONFLICT (installation_id) DO UPDATE
		SET access_token_ciphertext = EXCLUDED.access_token_ciphertext,
			refresh_token_ciphertext = EXCLUDED.refresh_token_ciphertext,
			expires_at = EXCLUDED.expires_at,
			token_version = oauth_credentials.token_version + 1,
			key_version = EXCLUDED.key_version,
			refreshed_at = now(), updated_at = now()`,
		installationID, accessCiphertext, refreshCiphertext, expiresAt, keyVersion,
	); err != nil {
		return InstallationResult{}, fmt.Errorf("save OAuth credentials: %w", err)
	}

	payload, _ := json.Marshal(map[string]string{"installation_id": installationID.String()})
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (installation_id, type, priority, payload)
		VALUES ($1, 'webhook.reconcile', 10, $2)`, installationID, payload); err != nil {
		return InstallationResult{}, fmt.Errorf("enqueue webhook reconcile: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (installation_id, actor_type, action, object_type, object_id)
		VALUES ($1, 'oauth', 'installation.authorized', 'installation', $2)`, installationID, installationID.String()); err != nil {
		return InstallationResult{}, fmt.Errorf("audit installation authorization: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return InstallationResult{}, fmt.Errorf("commit installation: %w", err)
	}
	return InstallationResult{ID: installationID, AccountID: account.ID, Status: "active"}, nil
}

func tokenExpiry(token Token) time.Time {
	base := time.Now().UTC()
	if token.ServerTime > 0 {
		serverTime := time.Unix(token.ServerTime, 0).UTC()
		if delta := serverTime.Sub(base); delta > -24*time.Hour && delta < 24*time.Hour {
			base = serverTime
		}
	}
	return base.Add(time.Duration(token.ExpiresIn) * time.Second)
}
