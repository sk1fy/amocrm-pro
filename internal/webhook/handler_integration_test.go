package webhook

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"golang.org/x/time/rate"
)

func TestWebhookIngressInstallationLimitIsIsolatedAndObservable(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	firstKey, secondKey := "fixture-webhook-a", "fixture-webhook-b"
	insertIngressInstallations(t, pool, firstKey, secondKey)

	registry := prometheus.NewRegistry()
	metrics := NewIngressMetrics(registry)
	limiter := testIngressLimiter(t, IngressLimiterConfig{
		GlobalRate: 100_000, GlobalBurst: 100,
		InstallationRate: rate.Limit(0.000001), InstallationBurst: 1,
		InactiveTTL: time.Hour,
	}, metrics)
	handler := ingressTestHandler(pool, limiter)

	assertWebhookStatus(t, handler, firstKey, 42, http.StatusNoContent, "")
	assertWebhookStatus(t, handler, firstKey, 42, http.StatusTooManyRequests, "1")
	assertWebhookStatus(t, handler, secondKey, 43, http.StatusNoContent, "")
	assertIngressRows(t, pool, 2, 2)

	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil),
	)
	body := recorder.Body.String()
	for _, series := range []string{
		`amocrm_webhook_ingress_limiter_decisions_total{outcome="rejected",scope="installation"} 1`,
		`amocrm_webhook_ingress_limiter_entries 2`,
	} {
		if !strings.Contains(body, series) {
			t.Fatalf("metrics do not contain %q:\n%s", series, body)
		}
	}
}

func TestWebhookIngressGlobalLimitRejectsBeforeDurableSideEffects(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	firstKey, secondKey := "fixture-global-a", "fixture-global-b"
	insertIngressInstallations(t, pool, firstKey, secondKey)

	limiter := testIngressLimiter(t, IngressLimiterConfig{
		GlobalRate: rate.Limit(0.000001), GlobalBurst: 1,
		InstallationRate: 100_000, InstallationBurst: 100,
		InactiveTTL: time.Hour,
	}, nil)
	handler := ingressTestHandler(pool, limiter)

	assertWebhookStatus(t, handler, firstKey, 42, http.StatusNoContent, "")
	assertWebhookStatus(t, handler, secondKey, 43, http.StatusTooManyRequests, "1")
	assertIngressRows(t, pool, 1, 1)
	if entries := limiter.installationCount(); entries != 1 {
		t.Fatalf("installation limiter entries = %d, want 1", entries)
	}
}

func TestWebhookIngressUnknownKeyConsumesGlobalCapacityBeforeLookup(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	validKey := "fixture-known-key"
	insertIngressInstallations(t, pool, validKey, "fixture-other-key")

	limiter := testIngressLimiter(t, IngressLimiterConfig{
		GlobalRate: rate.Limit(0.000001), GlobalBurst: 1,
		InstallationRate: 100_000, InstallationBurst: 100,
		InactiveTTL: time.Hour,
	}, nil)
	handler := ingressTestHandler(pool, limiter)

	assertWebhookStatus(t, handler, "fixture-unknown-key", 42, http.StatusNotFound, "")
	assertWebhookStatus(t, handler, validKey, 42, http.StatusTooManyRequests, "1")
	assertIngressRows(t, pool, 0, 0)
	if entries := limiter.installationCount(); entries != 0 {
		t.Fatalf("installation limiter entries = %d, want 0", entries)
	}
}

func testIngressLimiter(
	t *testing.T,
	config IngressLimiterConfig,
	metrics *IngressMetrics,
) *IngressLimiter {
	t.Helper()
	limiter, err := NewIngressLimiter(config, metrics)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func ingressTestHandler(pool *pgxpool.Pool, limiter *IngressLimiter) http.Handler {
	handler := NewHandler(
		installations.NewStore(pool), NewStore(pool),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		2<<20, time.Second, limiter,
	)
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Post("/hooks/{webhookKey}", handler.Receive)
	return router
}

func assertWebhookStatus(
	t *testing.T,
	handler http.Handler,
	key string,
	accountID int64,
	expectedStatus int,
	expectedRetryAfter string,
) {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost,
		"/hooks/"+key,
		strings.NewReader("account[id]="+formatAccountID(accountID)),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != expectedStatus {
		t.Fatalf("POST webhook status = %d, want %d; body=%q", recorder.Code, expectedStatus, recorder.Body.String())
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != expectedRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", retryAfter, expectedRetryAfter)
	}
}

func insertIngressInstallations(t *testing.T, pool *pgxpool.Pool, firstKey, secondKey string) {
	t.Helper()
	integrationID := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO integrations (id,code,client_id,client_secret_ciphertext,redirect_uri)
		VALUES ($1,$2,$3,decode('00','hex'),'https://example.test/oauth')`,
		integrationID, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range []struct {
		key       string
		accountID int64
	}{
		{key: firstKey, accountID: 42},
		{key: secondKey, accountID: 43},
	} {
		keyHash := sha256.Sum256([]byte(fixture.key))
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO installations (
				id,integration_id,account_id,account_domain,status,
				webhook_key_hash,webhook_status
			) VALUES ($1,$2,$3,$4,'active',$5,'active')`,
			uuid.New(), integrationID, fixture.accountID,
			"tenant-"+formatAccountID(int64(index))+".amocrm.ru", keyHash[:]); err != nil {
			t.Fatal(err)
		}
	}
}

func assertIngressRows(t *testing.T, pool *pgxpool.Pool, deliveries, jobs int) {
	t.Helper()
	var actualDeliveries, actualJobs int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM webhook_deliveries),
		(SELECT count(*) FROM jobs WHERE type='webhook.parse')`).
		Scan(&actualDeliveries, &actualJobs); err != nil {
		t.Fatal(err)
	}
	if actualDeliveries != deliveries || actualJobs != jobs {
		t.Fatalf("durable ingress rows deliveries/jobs = %d/%d, want %d/%d",
			actualDeliveries, actualJobs, deliveries, jobs)
	}
}

func formatAccountID(value int64) string {
	return strconv.FormatInt(value, 10)
}
