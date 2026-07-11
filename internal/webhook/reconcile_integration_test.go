package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

const reconcilePublicBaseURL = "https://widgets.example.test"

type reconcileTokenProvider struct {
	integrationID uuid.UUID
}

func (p reconcileTokenProvider) Token(_ context.Context, installationID uuid.UUID) (amocrm.AccessToken, error) {
	return amocrm.AccessToken{
		InstallationID: installationID,
		IntegrationID:  p.integrationID,
		AccountID:      42,
		AccountDomain:  "tenant.amocrm.ru",
		Value:          "integration-test-token",
		TokenVersion:   1,
	}, nil
}

func (reconcileTokenProvider) RefreshIfCurrent(_ context.Context, observed amocrm.AccessToken) (amocrm.AccessToken, error) {
	return observed, nil
}

func (reconcileTokenProvider) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	return nil
}

type reconcileRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip reconcileRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type reconcileRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

type reconcileFixture struct {
	pool           *pgxpool.Pool
	installationID uuid.UUID
	integrationID  uuid.UUID
	webhookKey     string
	settings       []string
	store          *ReconcileStore
}

func TestReconcileJobHandlerIntegration(t *testing.T) {
	pool := testkit.Postgres(t)

	t.Run("already converged is a no-op", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		destination := fixture.destination()
		calls := 0
		handler := fixture.handler(t, func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", request.Method)
			}
			return reconcileResponse(http.StatusOK, `{"_embedded":{"webhooks":[{"id":7,"destination":"`+destination+`","disabled":false,"settings":["update_lead","add_lead"]}]}}`, nil), nil
		})

		result, err := handler(context.Background(), fixture.job(fixture.installationID))
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("converged subscription must only be listed, got %d requests", calls)
		}
		if string(result) != `{"status":"active"}` {
			t.Fatalf("unexpected result: %s", result)
		}
		fixture.assertWebhookState(t, "active", "")
	})

	t.Run("missing registration is created", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		var requests []reconcileRequest
		handler := fixture.handler(t, func(request *http.Request) (*http.Response, error) {
			var body []byte
			if request.Body != nil {
				var err error
				body, err = io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
			}
			requests = append(requests, reconcileRequest{
				Method: request.Method,
				Path:   request.URL.Path,
				Query:  request.URL.RawQuery,
				Body:   body,
			})
			switch request.Method {
			case http.MethodGet:
				return reconcileResponse(http.StatusOK, `{"_embedded":{"webhooks":[]}}`, nil), nil
			case http.MethodPost:
				return reconcileResponse(http.StatusCreated, `{"id":8}`, nil), nil
			default:
				t.Fatalf("unexpected method: %s", request.Method)
				return nil, nil
			}
		})

		if _, err := handler(context.Background(), fixture.job(fixture.installationID)); err != nil {
			t.Fatal(err)
		}
		if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPost {
			t.Fatalf("expected GET followed by POST, got %#v", requests)
		}
		if requests[0].Path != "/api/v4/webhooks" || !strings.Contains(requests[0].Query, "filter%5Bdestination%5D=") {
			t.Fatalf("unexpected list request: %#v", requests[0])
		}
		var spec amocrm.WebhookSpec
		if err := json.Unmarshal(requests[1].Body, &spec); err != nil {
			t.Fatalf("decode register request: %v", err)
		}
		if spec.Destination != fixture.destination() || !sameSettings(spec.Settings, fixture.settings) {
			t.Fatalf("unexpected registration: %#v", spec)
		}
		fixture.assertWebhookState(t, "active", "")
	})

	t.Run("rate limit is retryable and persisted", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		handler := fixture.handler(t, func(*http.Request) (*http.Response, error) {
			return reconcileResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": []string{"7"}}), nil
		})

		_, err := handler(context.Background(), fixture.job(fixture.installationID))
		var jobError *jobs.Error
		if !errors.As(err, &jobError) || !jobError.Retryable || jobError.Code != string(amocrm.ErrorRateLimited) || jobError.RetryAfter != 7*time.Second {
			t.Fatalf("unexpected retry classification: %#v", err)
		}
		fixture.assertWebhookState(t, "error", "amoCRM API returned HTTP 429 (rate_limited)")
	})

	t.Run("validation failure is permanent and persisted", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		handler := fixture.handler(t, func(*http.Request) (*http.Response, error) {
			return reconcileResponse(http.StatusUnprocessableEntity, "", nil), nil
		})

		_, err := handler(context.Background(), fixture.job(fixture.installationID))
		var jobError *jobs.Error
		if !errors.As(err, &jobError) || jobError.Retryable || jobError.Code != string(amocrm.ErrorValidation) {
			t.Fatalf("unexpected permanent classification: %#v", err)
		}
		fixture.assertWebhookState(t, "error", "amoCRM API returned HTTP 422 (validation)")
	})

	t.Run("tenant payload mismatch is rejected before IO", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		calls := 0
		handler := fixture.handler(t, func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected request")
		})

		_, err := handler(context.Background(), fixture.job(uuid.New()))
		var jobError *jobs.Error
		if !errors.As(err, &jobError) || jobError.Retryable || jobError.Code != "tenant_scope_mismatch" {
			t.Fatalf("unexpected tenant mismatch classification: %#v", err)
		}
		if calls != 0 {
			t.Fatalf("tenant mismatch made %d HTTP requests", calls)
		}
		fixture.assertWebhookState(t, "pending", "stale error")
	})

	t.Run("persisted transport error is sanitized", func(t *testing.T) {
		fixture := newReconcileFixture(t, pool)
		unsafeMessage := "upstream\x00" + strings.Repeat("€", 600)
		handler := fixture.handler(t, func(*http.Request) (*http.Response, error) {
			return nil, errors.New(unsafeMessage)
		})

		if _, err := handler(context.Background(), fixture.job(fixture.installationID)); err == nil {
			t.Fatal("expected transport error")
		}
		status, message := fixture.webhookState(t)
		if status != "error" {
			t.Fatalf("unexpected webhook status: %s", status)
		}
		if strings.ContainsRune(message, '\x00') || !utf8.ValidString(message) || len(message) > 1000 {
			t.Fatalf("persisted error was not sanitized: bytes=%d valid=%v", len(message), utf8.ValidString(message))
		}
		if strings.Contains(message, fixture.webhookKey) {
			t.Fatal("persisted transport error leaked the webhook key")
		}
		if !strings.Contains(message, "upstream") {
			t.Fatalf("persisted error lost useful context: %q", message)
		}
	})
}

func newReconcileFixture(t *testing.T, pool *pgxpool.Pool) reconcileFixture {
	t.Helper()
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := uuid.New()
	installationID := uuid.New()
	keyRing, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x42}, cryptox.KeySize)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	webhookKey := "integration-test-webhook-key"
	keyCiphertext, keyVersion, err := keyRing.Seal([]byte(webhookKey), cryptox.InstallationWebhookKeyAAD(installationID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri, webhook_events
		) VALUES ($1, $2, $3, decode('00','hex'),
			'https://example.test/oauth', '["add_lead","update_lead"]'::jsonb)`,
		integrationID, "reconcile-"+integrationID.String(), uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status,
			webhook_key_ciphertext, webhook_key_key_version,
			webhook_status, webhook_settings, webhook_last_error
		) VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active', $3, $4,
			'pending', '["add_lead","update_lead"]'::jsonb, 'stale error')`,
		installationID, integrationID, keyCiphertext, keyVersion); err != nil {
		t.Fatal(err)
	}
	return reconcileFixture{
		pool:           pool,
		installationID: installationID,
		integrationID:  integrationID,
		webhookKey:     webhookKey,
		settings:       []string{"add_lead", "update_lead"},
		store:          NewReconcileStore(pool, keyRing),
	}
}

func (f reconcileFixture) handler(t *testing.T, roundTrip reconcileRoundTripper) jobs.Handler {
	t.Helper()
	client := amocrm.NewClient(&http.Client{Transport: roundTrip, Timeout: 2 * time.Second}, reconcileTokenProvider{
		integrationID: f.integrationID,
	})
	handler, err := ReconcileJobHandler(f.store, client, reconcilePublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func (f reconcileFixture) job(payloadInstallationID uuid.UUID) jobs.Job {
	payload, err := json.Marshal(map[string]string{"installation_id": payloadInstallationID.String()})
	if err != nil {
		panic(err)
	}
	return jobs.Job{
		ID:             uuid.New(),
		InstallationID: &f.installationID,
		Type:           "webhook.reconcile",
		Payload:        payload,
		Attempts:       1,
		MaxAttempts:    5,
	}
}

func (f reconcileFixture) destination() string {
	return reconcilePublicBaseURL + "/hooks/amocrm/v1/" + f.webhookKey
}

func (f reconcileFixture) webhookState(t *testing.T) (string, string) {
	t.Helper()
	var status string
	var message *string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT webhook_status, webhook_last_error
		FROM installations
		WHERE id = $1`, f.installationID).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if message == nil {
		return status, ""
	}
	return status, *message
}

func (f reconcileFixture) assertWebhookState(t *testing.T, wantStatus, wantMessage string) {
	t.Helper()
	status, message := f.webhookState(t)
	if status != wantStatus || message != wantMessage {
		t.Fatalf("unexpected webhook state: status=%q error=%q", status, message)
	}
}

func reconcileResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
