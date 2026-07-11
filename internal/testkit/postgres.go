package testkit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testLockID int64 = 7_614_553_921_011_044_221

func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; run make integration-test")
	}
	if err := validateResetTarget(databaseURL, os.Getenv("TEST_DATABASE_RESET_ALLOWED")); err != nil {
		t.Fatalf("unsafe integration database configuration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create test database pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)

	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test lock connection: %v", err)
	}
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", testLockID); err != nil {
		connection.Release()
		t.Fatalf("acquire test advisory lock: %v", err)
	}
	t.Cleanup(func() {
		unlockContext, unlockCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer unlockCancel()
		_, _ = connection.Exec(unlockContext, "SELECT pg_advisory_unlock($1)", testLockID)
		connection.Release()
	})
	return pool
}

func validateResetTarget(databaseURL, resetAllowed string) error {
	if resetAllowed != "true" {
		return fmt.Errorf("TEST_DATABASE_RESET_ALLOWED must equal true")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	if !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		return fmt.Errorf("database name %q must end with _test", config.ConnConfig.Database)
	}
	return nil
}

func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
		TRUNCATE TABLE
			audit_log, idempotency_keys, used_widget_tokens,
			job_attempts, outbound_effects, workflow_runs,
			lead_status_workflow_rules, jobs, inbox_events,
			webhook_event_tombstones, webhook_deliveries,
			oauth_credentials, oauth_states, installations, integrations
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset test database: %v", err)
	}
}
