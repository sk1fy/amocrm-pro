package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestUpdatedAtUsesStatementTimestamp(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var integrationID string
	var transactionStarted time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO integrations (
			code, client_id, client_secret_ciphertext, redirect_uri
		) VALUES ('statement-time-test', 'statement-time-client', decode('01', 'hex'), 'https://example.invalid/callback')
		RETURNING id, transaction_timestamp()
	`).Scan(&integrationID, &transactionStarted); err != nil {
		t.Fatalf("insert integration: %v", err)
	}

	if _, err := tx.Exec(ctx, "SELECT pg_sleep(0.1)"); err != nil {
		t.Fatalf("delay within transaction: %v", err)
	}

	var updatedAt time.Time
	var statementAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE integrations
		SET settings = jsonb_build_object('updated', true)
		WHERE id = $1
		RETURNING updated_at, statement_timestamp()
	`, integrationID).Scan(&updatedAt, &statementAt); err != nil {
		t.Fatalf("update integration: %v", err)
	}

	if !updatedAt.Equal(statementAt) {
		t.Fatalf("updated_at = %s, want statement timestamp %s", updatedAt, statementAt)
	}
	if !updatedAt.After(transactionStarted) {
		t.Fatalf("updated_at = %s, want after transaction start %s", updatedAt, transactionStarted)
	}
}
