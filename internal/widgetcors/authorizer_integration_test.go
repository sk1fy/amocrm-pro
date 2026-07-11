package widgetcors

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestPostgresAuthorizerRequiresActiveInstallationAndIntegration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := uuid.New()
	installationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri
		) VALUES ($1, 'widget-cors', $2, decode('00','hex'), 'https://api.example.test/oauth')`,
		integrationID, uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status
		) VALUES ($1, $2, 42, 'Tenant.AmoCRM.ru', 'active')`,
		installationID, integrationID); err != nil {
		t.Fatal(err)
	}

	authorizer := NewPostgresAuthorizer(pool)
	assertActiveOrigin(t, authorizer, activeOrigin, true)
	assertActiveOrigin(t, authorizer, "https://other.amocrm.ru", false)
	assertActiveOrigin(t, authorizer, "http://tenant.amocrm.ru", false)

	if _, err := pool.Exec(ctx, `UPDATE installations SET status='uninstalled' WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	assertActiveOrigin(t, authorizer, activeOrigin, false)
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, integrationID); err != nil {
		t.Fatal(err)
	}
	assertActiveOrigin(t, authorizer, activeOrigin, false)
}

func assertActiveOrigin(t *testing.T, authorizer OriginAuthorizer, origin string, want bool) {
	t.Helper()
	active, err := authorizer.IsActiveOrigin(context.Background(), origin)
	if err != nil {
		t.Fatalf("IsActiveOrigin(%q): %v", origin, err)
	}
	if active != want {
		t.Fatalf("IsActiveOrigin(%q) = %t, want %t", origin, active, want)
	}
}
