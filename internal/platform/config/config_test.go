package config

import (
	"strings"
	"testing"
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
