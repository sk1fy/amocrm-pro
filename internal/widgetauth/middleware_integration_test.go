package widgetauth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestMiddlewareAuthenticatesBothHeaderContractsWithPostgresReplayStore(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)

	// Replay rows use PostgreSQL's clock for created_at and require a later
	// expiry, so token timestamps must use the same clock instead of a fixed date.
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	integrationID := uuid.New()
	installationID := uuid.New()
	clientID := uuid.NewString()
	secret := []byte("private-widget-integration-test-secret")
	ring, err := cryptox.NewKeyRing(
		map[int][]byte{1: bytes.Repeat([]byte{0x42}, cryptox.KeySize)},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, keyVersion, err := ring.Seal(secret, cryptox.IntegrationSecretAAD(integrationID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext,
			client_secret_key_version, redirect_uri
		) VALUES ($1, 'widget-auth-http', $2, $3, $4,
			'https://backend.example.test/oauth/amocrm/callback')`,
		integrationID, clientID, ciphertext, keyVersion,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status
		) VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active')`,
		installationID, integrationID,
	); err != nil {
		t.Fatal(err)
	}

	authenticator, err := NewAuthenticator(
		NewStore(pool), ring, WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := Middleware(authenticator)(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromContext(request.Context())
		if !ok || principal.InstallationID != installationID || principal.UserID != 84 {
			t.Fatalf("principal = %+v/%t", principal, ok)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	sign := func(tokenID string) string {
		t.Helper()
		claims := jwt.MapClaims{
			"iss":         "https://tenant.amocrm.ru",
			"aud":         "https://backend.example.test",
			"jti":         tokenID,
			"iat":         now.Add(-time.Minute).Unix(),
			"nbf":         now.Add(-time.Minute).Unix(),
			"exp":         now.Add(10 * time.Minute).Unix(),
			"account_id":  int64(42),
			"user_id":     int64(84),
			"client_uuid": clientID,
			"subdomain":   "tenant",
		}
		rawToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}
		return rawToken
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil)
	request.Header.Set("X-Auth-Token", sign(uuid.NewString()))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("X-Auth-Token status/body = %d/%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer "+sign(uuid.NewString()))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("Bearer status/body = %d/%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil)
	request.Header.Set("X-Auth-Token", sign(uuid.NewString()))
	request.Header.Set("Authorization", "Bearer "+sign(uuid.NewString()))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("ambiguous status/body = %d/%q", response.Code, response.Body.String())
	}

	var usedTokens int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM used_widget_tokens`).Scan(&usedTokens); err != nil {
		t.Fatal(err)
	}
	if usedTokens != 2 {
		t.Fatalf("used widget tokens = %d, want 2", usedTokens)
	}
}
