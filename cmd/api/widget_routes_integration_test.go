package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

func TestWidgetRoutesRateLimitBeforeConsumptionAcrossIntegrations(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	type tenant struct {
		integration, installation uuid.UUID
		client, secret            string
	}
	var tenants []tenant
	for _, code := range []string{"widget-limit-a", "widget-limit-b"} {
		item := tenant{client: uuid.NewString(), secret: "synthetic-" + uuid.NewString(), installation: uuid.New()}
		integration, err := oauth.NewStore(pool, keys).EnsureIntegration(ctx, oauth.IntegrationInput{
			Code: code, ClientID: item.client, ClientSecret: item.secret, RedirectURI: "https://backend.example.test/oauth/amocrm/callback", WebhookEvents: []string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		item.integration = integration.ID
		if _, err := pool.Exec(ctx, `INSERT INTO installations (id,integration_id,account_id,account_domain,status)
			VALUES ($1,$2,42,'shared.amocrm.ru','active')`, item.installation, item.integration); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, item)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(integration_id, account_id, account_domain, status)
		VALUES ($1,43,'other.amocrm.ru','active')`, tenants[1].integration); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	auth, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys, widgetauth.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	newLimiter := func() *widgetlimit.Limiter {
		t.Helper()
		limiter, err := widgetlimit.New(widgetlimit.Config{IntegrationRate: 0.01, IntegrationBurst: 2,
			InstallationRate: 0.01, InstallationBurst: 1, InactiveTTL: time.Hour, MaxEntries: 10}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return limiter
	}
	limiter := newLimiter()
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	store := jobs.NewStore(pool)
	handler := widgetapi.NewHandler(store, widgetapi.NewActionStore(pool, store))
	read := protectWidgetRoute(auth, limiter, cors, true, http.HandlerFunc(handler.Bootstrap))
	action := protectWidgetRoute(auth, limiter, cors, false, http.HandlerFunc(handler.Ping))
	sign := func(item tenant, jti string) string {
		t.Helper()
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": "https://shared.amocrm.ru", "aud": "https://backend.example.test", "jti": jti,
			"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
			"account_id": 42, "user_id": 71, "client_uuid": item.client,
		}).SignedString([]byte(item.secret))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := func(target http.Handler, method, raw string, origin ...string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/widget", nil)
		r.Header.Set("Origin", "https://shared.amocrm.ru")
		if len(origin) > 0 {
			r.Header.Set("Origin", origin[0])
		}
		if raw != "" {
			r.Header.Set("X-Auth-Token", raw)
		}
		if method == http.MethodOptions {
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "X-Auth-Token, Idempotency-Key")
		}
		r.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		target.ServeHTTP(w, r)
		return w
	}
	// Preflight and invalid credentials allocate no tenant budget.
	if w := request(action, http.MethodOptions, ""); w.Code != 204 {
		t.Fatalf("preflight=%d", w.Code)
	}
	for range 3 {
		if w := request(read, http.MethodGet, "invalid-token"); w.Code != 401 {
			t.Fatalf("invalid JWT=%d", w.Code)
		}
	}
	if w := request(read, http.MethodGet, ""); w.Code != 401 {
		t.Fatalf("missing credential=%d", w.Code)
	}
	wrong := tenants[0]
	wrong.secret = "incorrect-signing-secret"
	if w := request(read, http.MethodGet, sign(wrong, "wrong-signature")); w.Code != 401 {
		t.Fatalf("wrong signature=%d", w.Code)
	}
	first := sign(tenants[0], "first-token")
	if w := request(read, http.MethodGet, first, "https://other.amocrm.ru"); w.Code != 403 {
		t.Fatalf("active origin/issuer mismatch=%d", w.Code)
	}
	if w := request(read, http.MethodGet, first); w.Code != 200 {
		t.Fatalf("initial read=%d %s", w.Code, w.Body.String())
	}
	deferred := sign(tenants[0], "deferred-token")
	w := request(action, http.MethodPost, deferred)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" ||
		w.Header().Get("Access-Control-Allow-Origin") != "https://shared.amocrm.ru" ||
		w.Header().Get("Access-Control-Expose-Headers") != "X-Request-ID, Idempotency-Replayed, Retry-After" {
		t.Fatalf("rate response=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
	var tokens, keysCount, jobsCount int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM used_widget_tokens WHERE integration_id=$1),
		(SELECT count(*) FROM idempotency_keys WHERE installation_id=$2),
		(SELECT count(*) FROM jobs WHERE installation_id=$2)`, tenants[0].integration, tenants[0].installation).Scan(&tokens, &keysCount, &jobsCount); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 || keysCount != 0 || jobsCount != 0 {
		t.Fatalf("429 consumed durable state: %d/%d/%d", tokens, keysCount, jobsCount)
	}
	if w := request(action, http.MethodPost, sign(tenants[1], "deferred-token")); w.Code != 202 {
		t.Fatalf("A rate limit affected B=%d %s", w.Code, w.Body.String())
	}
	// A fresh process budget models quota becoming available; neither the JWT
	// rejected with 429 nor its idempotency key has been spent in PostgreSQL.
	action = protectWidgetRoute(auth, newLimiter(), cors, false, http.HandlerFunc(handler.Ping))
	if w := request(action, http.MethodPost, deferred); w.Code != 202 {
		t.Fatalf("retry rejected=%d %s", w.Code, w.Body.String())
	}
	read = protectWidgetRoute(auth, newLimiter(), cors, true, http.HandlerFunc(handler.Bootstrap))
	if w := request(read, http.MethodGet, first); w.Code != 401 {
		t.Fatalf("read-token replay accepted=%d", w.Code)
	}
}
