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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

var ErrNotActive = errors.New("webhook subscription is not on an active installation")

// Gateway is the amoCRM webhook surface used by reconcile and uninstall.
// *amocrm.Client implements it; tests inject fakes. HTTP semantics of
// List/Register/Delete stay those of the existing amoCRM client.
type Gateway interface {
	ListWebhooks(context.Context, uuid.UUID, string) ([]amocrm.Webhook, error)
	RegisterWebhook(context.Context, uuid.UUID, amocrm.WebhookSpec) (amocrm.Webhook, error)
	DeleteWebhook(context.Context, uuid.UUID, string) error
}

var _ Gateway = (*amocrm.Client)(nil)

type ReconcileStore struct {
	pool *pgxpool.Pool
	keys *cryptox.KeyRing
}

type subscription struct {
	InstallationID uuid.UUID
	WebhookKey     string
	Settings       []string
	Status         string
}

type webhookPlan struct {
	delete   []string
	register bool
}

func NewReconcileStore(pool *pgxpool.Pool, keys *cryptox.KeyRing) *ReconcileStore {
	return &ReconcileStore{pool: pool, keys: keys}
}

func (s *ReconcileStore) Load(ctx context.Context, installationID uuid.UUID) (subscription, error) {
	result, err := s.load(ctx, installationID)
	if err != nil {
		return subscription{}, err
	}
	if result.Status != "active" {
		return subscription{}, ErrNotActive
	}
	if result.WebhookKey == "" {
		return subscription{}, errors.New("load webhook subscription: webhook key is missing")
	}
	return result, nil
}

func (s *ReconcileStore) load(ctx context.Context, installationID uuid.UUID) (subscription, error) {
	var keyCiphertext []byte
	var keyVersion *int
	var settingsJSON json.RawMessage
	result := subscription{InstallationID: installationID}
	if err := s.pool.QueryRow(ctx, `
		SELECT webhook_key_ciphertext, webhook_key_key_version, webhook_settings, status
		FROM installations
		WHERE id = $1`, installationID,
	).Scan(&keyCiphertext, &keyVersion, &settingsJSON, &result.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription{}, fmt.Errorf("load webhook subscription: %w", err)
		}
		return subscription{}, fmt.Errorf("load webhook subscription: %w", err)
	}
	if len(keyCiphertext) > 0 && keyVersion != nil {
		key, err := s.keys.Open(*keyVersion, keyCiphertext, cryptox.InstallationWebhookKeyAAD(installationID))
		if err != nil {
			return subscription{}, fmt.Errorf("decrypt webhook key: %w", err)
		}
		defer clear(key)
		result.WebhookKey = string(key)
	}
	if err := json.Unmarshal(settingsJSON, &result.Settings); err != nil {
		return subscription{}, fmt.Errorf("decode webhook settings: %w", err)
	}
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

func (s *ReconcileStore) MarkUnregistered(ctx context.Context, installationID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE installations
		SET webhook_status = 'unregistered', webhook_checked_at = now(),
			webhook_last_error = NULL, updated_at = now()
		WHERE id = $1`, installationID,
	)
	if err != nil {
		return fmt.Errorf("mark webhook subscription unregistered: %w", err)
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

func (s *ReconcileStore) HasOAuthCredentials(ctx context.Context, installationID uuid.UUID) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_credentials WHERE installation_id=$1)`, installationID).Scan(&exists); err != nil {
		return false, fmt.Errorf("read oauth credentials: %w", err)
	}
	return exists, nil
}

func ReconcileJobHandler(
	store *ReconcileStore,
	client Gateway,
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
			if errors.Is(err, ErrNotActive) {
				return nil, jobs.Permanent("installation_not_active", err)
			}
			return nil, err
		}
		destination := webhookDestination(base, subscription.WebhookKey)
		if err := store.synchronize(ctx, client, subscription, destination, false); err != nil {
			recordReconcileError(store, subscription.InstallationID, err)
			return nil, classifyReconcileError(err)
		}
		if err := store.MarkActive(ctx, subscription.InstallationID, subscription.Settings); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":"active"}`), nil
	}, nil
}

// Unregister deletes only registered destinations owned by this installation,
// including legacy URLs carrying its exact webhook secret.
func Unregister(ctx context.Context, store *ReconcileStore, client Gateway, installationID uuid.UUID) error {
	sub, err := store.load(ctx, installationID)
	if err != nil {
		return err
	}
	if sub.Status != "uninstalled" {
		return ErrNotActive
	}

	hasCredentials, err := store.HasOAuthCredentials(ctx, installationID)
	if err != nil {
		return err
	}
	if hasCredentials {
		if err := store.synchronize(ctx, client, sub, "", true); err != nil {
			recordReconcileError(store, installationID, err)
			return classifyReconcileError(err)
		}
	}
	if err := store.MarkUnregistered(ctx, installationID); err != nil {
		return err
	}
	return nil
}

func syncInstallationWebhooks(
	ctx context.Context,
	client Gateway,
	installationID uuid.UUID,
	desired string,
	settings []string,
	unregister bool,
	owned ...string,
) error {
	actual, err := client.ListWebhooks(ctx, installationID, "")
	if err != nil {
		return err
	}
	plan := planManagedWebhooks(desired, settings, actual, unregister, owned...)
	return applyWebhookPlan(ctx, client, installationID, plan, settings, desired)
}

func planManagedWebhooks(desired string, settings []string, actual []amocrm.Webhook, unregister bool, owned ...string) webhookPlan {
	allowed := map[string]bool{}
	for _, destination := range owned {
		allowed[destination] = true
	}
	if desired != "" {
		allowed[desired] = true
	}
	copies := map[string][]amocrm.Webhook{}
	for _, registered := range actual {
		if registered.Destination == "" || !allowed[registered.Destination] {
			continue
		}
		copies[registered.Destination] = append(copies[registered.Destination], registered)
	}
	plan := webhookPlan{}
	if unregister {
		for destination := range copies {
			plan.delete = append(plan.delete, destination)
		}
		slices.Sort(plan.delete)
		return plan
	}
	if _, found := copies[desired]; !found {
		plan.register = true
	}
	for destination, registered := range copies {
		if destination != desired {
			plan.delete = append(plan.delete, destination)
			continue
		}
		good := 0
		for _, hook := range registered {
			if !hook.Disabled && sameSettings(hook.Settings, settings) {
				good++
			}
		}
		if good != 1 || len(registered) != 1 {
			plan.delete = append(plan.delete, destination)
			plan.register = true
		}
	}
	slices.Sort(plan.delete)
	return plan
}

func applyWebhookPlan(
	ctx context.Context,
	client Gateway,
	installationID uuid.UUID,
	plan webhookPlan,
	settings []string,
	desired string,
) error {
	for _, destination := range plan.delete {
		if err := deleteWebhook(ctx, client, installationID, destination); err != nil {
			return err
		}
	}
	if plan.register {
		if _, err := client.RegisterWebhook(ctx, installationID, amocrm.WebhookSpec{
			Destination: desired,
			Settings:    settings,
		}); err != nil {
			return err
		}
	}
	return nil
}

func deleteWebhook(ctx context.Context, client Gateway, installationID uuid.UUID, destination string) error {
	err := client.DeleteWebhook(ctx, installationID, destination)
	var apiError *amocrm.APIError
	if errors.As(err, &apiError) && apiError.Kind == amocrm.ErrorNotFound {
		return nil
	}
	return err
}

func webhookDestination(base *url.URL, webhookKey string) string {
	return strings.TrimSuffix(base.String(), "/") + "/hooks/amocrm/v1/" + url.PathEscape(webhookKey)
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
	if errors.Is(err, ErrNotActive) {
		return jobs.Permanent("installation_not_active", err)
	}
	var apiError *amocrm.APIError
	if errors.As(err, &apiError) && apiError.Retryable {
		return jobs.Retryable(string(apiError.Kind), apiError.RetryAfter, err)
	}
	if errors.As(err, &apiError) {
		return jobs.Permanent(string(apiError.Kind), err)
	}
	return err
}
