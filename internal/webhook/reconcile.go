package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

type ReconcileStore struct {
	pool *pgxpool.Pool
	keys *cryptox.KeyRing
}

type subscription struct {
	InstallationID uuid.UUID
	WebhookKey     string
	Settings       []string
}

func NewReconcileStore(pool *pgxpool.Pool, keys *cryptox.KeyRing) *ReconcileStore {
	return &ReconcileStore{pool: pool, keys: keys}
}

func (s *ReconcileStore) Load(ctx context.Context, installationID uuid.UUID) (subscription, error) {
	var keyCiphertext []byte
	var keyVersion int
	var settingsJSON json.RawMessage
	result := subscription{InstallationID: installationID}
	if err := s.pool.QueryRow(ctx, `
		SELECT webhook_key_ciphertext, webhook_key_key_version, webhook_settings
		FROM installations
		WHERE id = $1 AND status = 'active'`, installationID,
	).Scan(&keyCiphertext, &keyVersion, &settingsJSON); err != nil {
		return subscription{}, fmt.Errorf("load webhook subscription: %w", err)
	}
	key, err := s.keys.Open(keyVersion, keyCiphertext, cryptox.InstallationWebhookKeyAAD(installationID))
	if err != nil {
		return subscription{}, fmt.Errorf("decrypt webhook key: %w", err)
	}
	defer clear(key)
	if err := json.Unmarshal(settingsJSON, &result.Settings); err != nil {
		return subscription{}, fmt.Errorf("decode webhook settings: %w", err)
	}
	result.WebhookKey = string(key)
	return result, nil
}

func (s *ReconcileStore) MarkActive(ctx context.Context, installationID uuid.UUID, settings []string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE installations
		SET webhook_status = 'active', webhook_settings = $2,
			webhook_checked_at = now(), webhook_last_error = NULL, updated_at = now()
		WHERE id = $1`, installationID, settings,
	)
	if err != nil {
		return fmt.Errorf("mark webhook subscription active: %w", err)
	}
	return nil
}

func (s *ReconcileStore) MarkError(ctx context.Context, installationID uuid.UUID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE installations
		SET webhook_status = 'error', webhook_checked_at = now(),
			webhook_last_error = $2, updated_at = now()
		WHERE id = $1`, installationID, sanitize.Text(message, 1000),
	)
	if err != nil {
		return fmt.Errorf("mark webhook subscription error: %w", err)
	}
	return nil
}

func ReconcileJobHandler(
	store *ReconcileStore,
	client *amocrm.Client,
	publicBaseURL string,
) (jobs.Handler, error) {
	base, err := validatePublicBaseURL(publicBaseURL)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if job.InstallationID == nil {
			return nil, jobs.Permanent("invalid_tenant_scope", errors.New("webhook reconcile job has no installation"))
		}
		var payload struct {
			InstallationID string `json:"installation_id"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, jobs.Permanent("invalid_payload", err)
		}
		payloadInstallationID, err := uuid.Parse(payload.InstallationID)
		if err != nil || payloadInstallationID != *job.InstallationID {
			return nil, jobs.Permanent("tenant_scope_mismatch", errors.New("reconcile payload installation mismatch"))
		}
		subscription, err := store.Load(ctx, *job.InstallationID)
		if err != nil {
			return nil, err
		}
		destination := strings.TrimSuffix(base.String(), "/") + "/hooks/amocrm/v1/" + url.PathEscape(subscription.WebhookKey)
		actual, err := client.ListWebhooks(ctx, subscription.InstallationID, destination)
		if err != nil {
			recordReconcileError(store, subscription.InstallationID, err)
			return nil, classifyReconcileError(err)
		}
		converged := false
		for _, registered := range actual {
			if registered.Destination == destination && !registered.Disabled && sameSettings(registered.Settings, subscription.Settings) {
				converged = true
				break
			}
		}
		if !converged {
			if _, err := client.RegisterWebhook(ctx, subscription.InstallationID, amocrm.WebhookSpec{
				Destination: destination,
				Settings:    subscription.Settings,
			}); err != nil {
				recordReconcileError(store, subscription.InstallationID, err)
				return nil, classifyReconcileError(err)
			}
		}
		if err := store.MarkActive(ctx, subscription.InstallationID, subscription.Settings); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":"active"}`), nil
	}, nil
}

func recordReconcileError(store *ReconcileStore, installationID uuid.UUID, reconcileErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = store.MarkError(ctx, installationID, reconcileErr.Error())
}

func validatePublicBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("PUBLIC_BASE_URL must be an absolute HTTPS URL")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("PUBLIC_BASE_URL must not contain a path")
	}
	return &url.URL{Scheme: "https", Host: parsed.Host}, nil
}

func sameSettings(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func classifyReconcileError(err error) error {
	var apiError *amocrm.APIError
	if errors.As(err, &apiError) && apiError.Retryable {
		return jobs.Retryable(string(apiError.Kind), apiError.RetryAfter, err)
	}
	if errors.As(err, &apiError) {
		return jobs.Permanent(string(apiError.Kind), err)
	}
	return err
}
