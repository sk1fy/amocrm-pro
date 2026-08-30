package widgetcors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestPostgresAuthorizerRequiresActiveInstallationAndIntegration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := uuid.New()
	installationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri
		) VALUES ($1, 'widget-cors', $2, decode('00','hex'), 'https://api.example.test/oauth')`,
		integrationID, uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status
		) VALUES ($1, $2, 42, 'Tenant.AmoCRM.ru', 'active')`,
		installationID, integrationID); err != nil {
		t.Fatal(err)
	}

	authorizer := NewPostgresAuthorizer(pool)
	assertActiveOrigin(t, authorizer, activeOrigin, true)
	assertActiveOrigin(t, authorizer, "https://other.amocrm.ru", false)
	assertActiveOrigin(t, authorizer, "http://tenant.amocrm.ru", false)

	handler := Middleware(authorizer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight reached application handler")
	}))
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/widget/bootstrap", nil)
	request.Header.Set("Origin", activeOrigin)
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "X-Auth-Token, X-Request-ID")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("X-Auth-Token preflight status/body = %d/%q", response.Code, response.Body.String())
	}
	assertHeader(t, response.Header(), allowHeadersHeader, "X-Auth-Token, X-Request-ID")

	if _, err := pool.Exec(ctx, `UPDATE installations SET status='uninstalled' WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	assertActiveOrigin(t, authorizer, activeOrigin, false)
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, integrationID); err != nil {
		t.Fatal(err)
	}
	assertActiveOrigin(t, authorizer, activeOrigin, false)
}

func assertActiveOrigin(t *testing.T, authorizer OriginAuthorizer, origin string, want bool) {
	t.Helper()
	active, err := authorizer.IsActiveOrigin(context.Background(), origin)
	if err != nil {
		t.Fatalf("IsActiveOrigin(%q): %v", origin, err)
	}
	if active != want {
		t.Fatalf("IsActiveOrigin(%q) = %t, want %t", origin, active, want)
	}
}
