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
