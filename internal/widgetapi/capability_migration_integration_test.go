package widgetapi

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestCapabilityMigrationBackfillsOnlyExistingIntegrations(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	existing := widgetPrincipal(t, pool, 304, 74)
	// DDL is transactional: reconstruct the pre-capability schema and exercise
	// the real migration without changing the shared migration ledger.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, name := range []string{
		"../../migrations/000007_integration_services.down.sql",
		"../../migrations/000007_integration_services.up.sql",
	} {
		sql, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM integration_services WHERE integration_id=$1 AND service_code='lead-status'`, existing.IntegrationID).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("existing integration grant = %t, %v", enabled, err)
	}
	newID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO integrations (id,code,client_id,client_secret_ciphertext,redirect_uri)
		VALUES ($1,$2,$3,decode('00','hex'),'https://example.test/oauth')`, newID, "new-"+newID.String(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM integration_services WHERE integration_id=$1`, newID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("new integration implicit grants = %d, %v", grants, err)
	}
}
