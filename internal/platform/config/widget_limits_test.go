package config

import (
	"testing"
	"time"
)

func TestWidgetLimitAndWorkerFairnessDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("APP_ENV", "development")
	t.Setenv("PUBLIC_BASE_URL", "https://backend.example.test")
	a, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if a.WidgetIntegrationRate != 100 || a.WidgetIntegrationBurst != 200 || a.WidgetInstallationRate != 10 ||
		a.WidgetInstallationBurst != 20 || a.WidgetLimiterInactiveTTL != 10*time.Minute || a.WidgetLimiterMaxEntries != 10_000 {
		t.Fatalf("unexpected widget defaults: %+v", a)
	}
	w, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if w.IntegrationConcurrency != 2 {
		t.Fatalf("integration concurrency=%d", w.IntegrationConcurrency)
	}
}

func TestWidgetLimiterRejectsInvalidRuntimeConfiguration(t *testing.T) {
	for _, pair := range [][2]string{
		{"WIDGET_INTEGRATION_RATE_PER_SECOND", "NaN"}, {"WIDGET_INSTALLATION_RATE_PER_SECOND", "0"},
		{"WIDGET_INTEGRATION_BURST", "0"}, {"WIDGET_INSTALLATION_BURST", "0"},
		{"WIDGET_LIMITER_MAX_ENTRIES", "0"}, {"WIDGET_LIMITER_INACTIVE_TTL", "1s"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
			t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
			t.Setenv("APP_ENV", "development")
			t.Setenv(pair[0], pair[1])
			if _, err := LoadAPI(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, value := range []string{"0", "-1", "65"} {
		t.Run("worker_"+value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
			t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
			t.Setenv("APP_ENV", "development")
			t.Setenv("WORKER_INTEGRATION_CONCURRENCY", value)
			if _, err := LoadWorker(); err == nil {
				t.Fatal("invalid fairness cap accepted")
			}
		})
	}
}
