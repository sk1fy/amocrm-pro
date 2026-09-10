package config

import (
	"strings"
	"testing"
	"time"
)

func TestProductionRejectsPublicDevelopmentEncryptionKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "production")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	_, err := LoadAPI()
	if err == nil || !strings.Contains(err.Error(), "development encryption key") {
		t.Fatalf("expected development-key rejection, got %v", err)
	}
}

func TestAPIManagementAddressDefaultsToSeparateListener(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)

	api, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if api.HTTPAddress != ":8080" || api.ManagementHTTPAddress != ":8082" {
		t.Fatalf("unexpected API listeners: public=%q management=%q", api.HTTPAddress, api.ManagementHTTPAddress)
	}
}

func TestAPIRejectsConflictingManagementAddress(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("HTTP_ADDRESS", "0.0.0.0:8080")
	t.Setenv("MANAGEMENT_HTTP_ADDRESS", ":8080")

	_, err := LoadAPI()
	if err == nil || !strings.Contains(err.Error(), "must not conflict") {
		t.Fatalf("expected listener conflict rejection, got %v", err)
	}
}

func TestAPIAcceptsCustomManagementAddress(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("MANAGEMENT_HTTP_ADDRESS", "127.0.0.1:9090")

	api, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if api.ManagementHTTPAddress != "127.0.0.1:9090" {
		t.Fatalf("management address = %q", api.ManagementHTTPAddress)
	}
}

func TestAPIRejectsInvalidManagementAddress(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("MANAGEMENT_HTTP_ADDRESS", ":invalid")

	_, err := LoadAPI()
	if err == nil || !strings.Contains(err.Error(), "port must be an integer") {
		t.Fatalf("expected management address rejection, got %v", err)
	}
}

func TestAPIWebhookLimiterDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)

	api, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if api.WebhookGlobalRate != 500 || api.WebhookGlobalBurst != 1_000 ||
		api.WebhookInstallationRate != 20 || api.WebhookInstallationBurst != 40 ||
		api.WebhookLimiterInactiveTTL != time.Hour {
		t.Fatalf("unexpected webhook limiter defaults: %+v", api)
	}
}

func TestAPIRejectsInvalidWebhookLimiterConfiguration(t *testing.T) {
	for _, testCase := range []struct {
		name, variable, value, errorText string
	}{
		{name: "zero global rate", variable: "WEBHOOK_GLOBAL_RATE_PER_SECOND", value: "0", errorText: "finite number"},
		{name: "non-finite installation rate", variable: "WEBHOOK_INSTALLATION_RATE_PER_SECOND", value: "NaN", errorText: "finite number"},
		{name: "zero global burst", variable: "WEBHOOK_GLOBAL_BURST", value: "0", errorText: "integer between"},
		{name: "zero installation burst", variable: "WEBHOOK_INSTALLATION_BURST", value: "0", errorText: "integer between"},
		{name: "hot inactive ttl", variable: "WEBHOOK_LIMITER_INACTIVE_TTL", value: "30s", errorText: "at least 1m"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
			t.Setenv("APP_ENV", "development")
			t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
			t.Setenv(testCase.variable, testCase.value)

			_, err := LoadAPI()
			if err == nil || !strings.Contains(err.Error(), testCase.errorText) {
				t.Fatalf("expected %s rejection, got %v", testCase.variable, err)
			}
		})
	}
}

func TestWorkerCleanupDefaultsAndZeroSafetyMargin(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("PUBLIC_BASE_URL", "https://hooks.example.test")
	t.Setenv("CLEANUP_SAFETY_MARGIN", "0s")
	worker, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if worker.CleanupInterval != 15*time.Minute || worker.CleanupTimeout != 30*time.Second ||
		worker.CleanupSafetyMargin != 0 || worker.CleanupBatchSize != 500 || worker.CleanupMaxBatches != 20 ||
		worker.WebhookInboxRetention != 720*time.Hour ||
		worker.WebhookDeliveryRetention != 720*time.Hour {
		t.Fatalf("unexpected cleanup defaults: %+v", worker)
	}
}

func TestWorkerRejectsNonPositiveWebhookRetention(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("PUBLIC_BASE_URL", "https://hooks.example.test")
	t.Setenv("WEBHOOK_INBOX_RETENTION", "0s")
	_, err := LoadWorker()
	if err == nil || !strings.Contains(err.Error(), "positive duration") {
		t.Fatalf("expected webhook retention rejection, got %v", err)
	}
}

func TestWorkerRejectsHotCleanupInterval(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("PUBLIC_BASE_URL", "https://hooks.example.test")
	t.Setenv("CLEANUP_INTERVAL", "100ms")
	_, err := LoadWorker()
	if err == nil || !strings.Contains(err.Error(), "at least 1s") {
		t.Fatalf("expected cleanup interval rejection, got %v", err)
	}
}

func TestWorkerRejectsSubsecondLease(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("PUBLIC_BASE_URL", "https://hooks.example.test")
	t.Setenv("WORKER_LEASE_DURATION", "1ns")
	_, err := LoadWorker()
	if err == nil || !strings.Contains(err.Error(), "at least 3s") {
		t.Fatalf("expected lease validation error, got %v", err)
	}
}

func TestWorkerReapBatchDefaultsAndBounds(t *testing.T) {
	setWorkerEnvironment(t)
	worker, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if worker.ReapBatchSize != 100 {
		t.Fatalf("reap batch size = %d, want 100", worker.ReapBatchSize)
	}

	for _, value := range []string{"0", "1001"} {
		t.Run(value, func(t *testing.T) {
			setWorkerEnvironment(t)
			t.Setenv("WORKER_REAP_BATCH_SIZE", value)
			_, err := LoadWorker()
			if err == nil || !strings.Contains(err.Error(), "WORKER_REAP_BATCH_SIZE") {
				t.Fatalf("expected reaper batch rejection for %q, got %v", value, err)
			}
		})
	}
}

func TestLoadRejectsInvalidEncryptionKeyRing(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:not-a-key")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected invalid ENCRYPTION_KEYS rejection")
	}
}

func TestAPIRejectsInvalidHTTPAddress(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("HTTP_ADDRESS", "8080")
	_, err := LoadAPI()
	if err == nil || !strings.Contains(err.Error(), "HTTP_ADDRESS") {
		t.Fatalf("expected HTTP_ADDRESS rejection, got %v", err)
	}
}

func TestAPIRejectsInvalidOAuthAndJWTDeadlines(t *testing.T) {
	for _, testCase := range []struct {
		name, variable, value, errorText string
	}{
		{name: "short oauth state", variable: "OAUTH_STATE_TTL", value: "30s", errorText: "at least 1m"},
		{name: "long oauth state", variable: "OAUTH_STATE_TTL", value: "2h", errorText: "at most 1h"},
		{name: "long jwt leeway", variable: "WIDGET_JWT_LEEWAY", value: "2m", errorText: "at most 1m"},
		{name: "long jwt lifetime", variable: "WIDGET_JWT_MAX_LIFETIME", value: "2h", errorText: "at most 1h"},
		{name: "zero oauth ip rate", variable: "OAUTH_IP_RATE_PER_SECOND", value: "0", errorText: "OAUTH_IP_RATE_PER_SECOND"},
		{name: "short oauth limiter ttl", variable: "OAUTH_LIMITER_INACTIVE_TTL", value: "1ms", errorText: "OAUTH_LIMITER_INACTIVE_TTL"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
			t.Setenv("APP_ENV", "development")
			t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
			t.Setenv(testCase.variable, testCase.value)
			_, err := LoadAPI()
			if err == nil || !strings.Contains(err.Error(), testCase.errorText) {
				t.Fatalf("expected %s rejection, got %v", testCase.variable, err)
			}
		})
	}
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("WIDGET_JWT_LEEWAY", "30s")
	t.Setenv("WIDGET_JWT_MAX_LIFETIME", "10s")
	_, err := LoadAPI()
	if err == nil || !strings.Contains(err.Error(), "WIDGET_JWT_MAX_LIFETIME must be at least WIDGET_JWT_LEEWAY") {
		t.Fatalf("expected jwt lifetime vs leeway rejection, got %v", err)
	}
}

func TestWorkerRejectsSubsecondJobTimeout(t *testing.T) {
	setWorkerEnvironment(t)
	t.Setenv("WORKER_JOB_TIMEOUT", "500ms")
	_, err := LoadWorker()
	if err == nil || !strings.Contains(err.Error(), "at least 1s") {
		t.Fatalf("expected job timeout rejection, got %v", err)
	}
}

func setWorkerEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("PUBLIC_BASE_URL", "https://hooks.example.test")
}
