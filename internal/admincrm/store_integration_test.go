package admincrm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestResolveConnectionPrefersRecentAuthorizedRealInstallation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := &Store{Pool: pool, Timeout: 2 * time.Second}
	ctx := t.Context()
	const account = int64(31415926)
	now := time.Now().UTC()

	active := insertIntegration(t, pool, "widget-fixture-a", "active")
	disabled := insertIntegration(t, pool, "widget-fixture-b", "disabled")
	older, newer := uuid.New(), uuid.New()
	insertInstallation(t, pool, older, active, account, "active", `{}`, now.Add(-time.Hour))
	insertCredentials(t, pool, older, now.Add(-2*time.Hour))
	insertInstallation(t, pool, newer, active, 27182818, "active", `{}`, now)
	insertCredentials(t, pool, newer, now)
	// Excluded rows: reauth required, disabled integration, fixture origin,
	// and an active installation without credentials.
	for _, row := range []struct {
		integration      uuid.UUID
		status, settings string
		credentials      bool
	}{
		{active, "reauth_required", `{}`, true},
		{disabled, "active", `{}`, true},
		{active, "active", `{"origin":"fixture"}`, true},
		{active, "active", `{}`, false},
	} {
		id := uuid.New()
		insertInstallationForIntegration(t, pool, id, row.integration, account, row.status, row.settings, now)
		if row.credentials {
			insertCredentials(t, pool, id, now)
		}
	}

	connection, err := store.ResolveConnection(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if connection.InstallationID != older || connection.IntegrationID != active || connection.IntegrationCode != "widget-fixture-a" {
		t.Fatalf("connection=%+v", connection)
	}

	recent := uuid.New()
	insertInstallation(t, pool, recent, active, account, "active", `{}`, now.Add(-3*time.Hour))
	insertCredentials(t, pool, recent, now.Add(-time.Minute))
	connection, err = store.ResolveConnection(ctx, account)
	if err != nil || connection.InstallationID != recent {
		t.Fatalf("most recently refreshed credentials must win: %+v %v", connection, err)
	}

	if _, err := store.ResolveConnection(ctx, 99999999); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account err=%v", err)
	}
	unauthorized := uuid.New()
	insertInstallation(t, pool, unauthorized, active, 11111111, "reauth_required", `{}`, now)
	if _, err := store.ResolveConnection(ctx, 11111111); !errors.Is(err, ErrNoAuthorizedConnection) {
		t.Fatalf("unauthorized account err=%v", err)
	}
}

func TestUsersReloadsAfterReauthorizationOfSameInstallation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := t.Context()
	const account = int64(31415926)
	installation := uuid.New()
	integration := insertIntegration(t, pool, "widget-fixture-a", "active")
	now := time.Now().UTC()
	insertInstallation(t, pool, installation, integration, account, "active", `{}`, now)
	insertCredentials(t, pool, installation, now)
	reader := &fakeReader{users: []amocrm.CRMUser{crmUser(1, "Первый", true, false)}}
	service := &Service{Resolver: &Store{Pool: pool}, CRM: reader}
	first, err := service.Users(ctx, account)
	if err != nil || len(first.Users) != 1 || first.Users[0].ID != 1 {
		t.Fatalf("before reauthorization: users=%+v err=%v", first.Users, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE oauth_credentials SET token_version = token_version + 1 WHERE installation_id = $1`, installation); err != nil {
		t.Fatal(err)
	}
	reader.users = []amocrm.CRMUser{crmUser(2, "Второй", true, false)}
	second, err := service.Users(ctx, account)
	if err != nil || second.Connection.InstallationID != installation || len(second.Users) != 1 || second.Users[0].ID != 2 {
		t.Fatalf("after reauthorization: users=%+v err=%v", second.Users, err)
	}
}

func insertIntegration(t *testing.T, pool *pgxpool.Pool, code, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO integrations (id, code, client_id, client_secret_ciphertext, redirect_uri, status)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, code, uuid.NewString(), []byte("fixture"), "https://backend.example.test/oauth/amocrm/callback", status); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertInstallation(t *testing.T, pool *pgxpool.Pool, id, integrationID uuid.UUID, accountID int64, status, settings string, updated time.Time) {
	t.Helper()
	insertInstallationForIntegration(t, pool, id, integrationID, accountID, status, settings, updated)
}

// Installations are unique per (integration, account); repeated rows in one
// account use a fresh integration of the same status so the fixture stays valid.
func insertInstallationForIntegration(t *testing.T, pool *pgxpool.Pool, id, integrationID uuid.UUID, accountID int64, status, settings string, updated time.Time) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM installations WHERE integration_id = $1 AND account_id = $2)`, integrationID, accountID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		var code, integrationStatus string
		if err := pool.QueryRow(context.Background(), `SELECT code, status FROM integrations WHERE id = $1`, integrationID).Scan(&code, &integrationStatus); err != nil {
			t.Fatal(err)
		}
		integrationID = insertIntegration(t, pool, code+"-"+id.String()[:8], integrationStatus)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installations (id, integration_id, account_id, account_domain, status, settings, created_at, updated_at)
		VALUES ($1, $2, $3, 'fixture-alpha.amocrm.test', $4, $5::jsonb, $6, $6)`,
		id, integrationID, accountID, status, settings, updated); err != nil {
		t.Fatal(err)
	}
}

func insertCredentials(t *testing.T, pool *pgxpool.Pool, installationID uuid.UUID, refreshed time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO oauth_credentials (
			installation_id, access_token_ciphertext, refresh_token_ciphertext,
			expires_at, token_version, key_version, created_at, refreshed_at
		) VALUES ($1, $2, $2, $3, 1, 1, $4, $4)`,
		installationID, []byte("fixture"), refreshed.Add(time.Hour), refreshed); err != nil {
		t.Fatal(err)
	}
}
