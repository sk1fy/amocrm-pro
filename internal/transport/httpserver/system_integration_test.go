package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestPublicAndManagementRouteIsolationWithPostgresReadiness(t *testing.T) {
	pool := testkit.Postgres(t)

	publicRouter := chi.NewRouter()
	RegisterPublicSystemRoutes(publicRouter)
	assertStatus(t, publicRouter, "/live", http.StatusOK)
	assertStatus(t, publicRouter, "/ready", http.StatusNotFound)
	assertStatus(t, publicRouter, "/metrics", http.StatusNotFound)

	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "amocrm_management_test_value"})
	gauge.Set(1)
	registry.MustRegister(gauge)
	managementRouter := chi.NewRouter()
	RegisterManagementRoutes(
		managementRouter,
		pool,
		time.Second,
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	)
	assertStatus(t, managementRouter, "/live", http.StatusOK)
	assertStatus(t, managementRouter, "/ready", http.StatusOK)

	metrics := httptest.NewRecorder()
	managementRouter.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "amocrm_management_test_value 1") {
		t.Fatalf("management metrics status/body = %d/%q", metrics.Code, metrics.Body.String())
	}
	if contentType := metrics.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("management metrics content type = %q", contentType)
	}

	unavailablePool := unavailableDatabasePool(t)
	unavailableRouter := chi.NewRouter()
	RegisterManagementRoutes(
		unavailableRouter,
		unavailablePool,
		time.Second,
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	)
	assertStatus(t, unavailableRouter, "/ready", http.StatusServiceUnavailable)
}

func unavailableDatabasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse integration database config: %v", err)
	}
	config.ConnConfig.Database = "amocrm_management_missing"
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("create unavailable database pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertStatus(t *testing.T, handler http.Handler, path string, expected int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != expected {
		t.Fatalf("GET %s status = %d, want %d; body=%q", path, recorder.Code, expected, recorder.Body.String())
	}
}
