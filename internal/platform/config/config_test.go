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
