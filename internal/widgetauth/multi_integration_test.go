package widgetauth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestAuthenticationIsolatesIntegrationsInSameAccount(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x51}, cryptox.KeySize)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	type tenant struct {
		integrationID  uuid.UUID
		installationID uuid.UUID
		clientID       string
		secret         []byte
	}
	tenants := make([]tenant, 2)
	for i := range tenants {
		tenants[i] = tenant{uuid.New(), uuid.New(), uuid.NewString(), []byte("synthetic-secret-" + uuid.NewString())}
		item := tenants[i]
		ciphertext, version, err := keys.Seal(item.secret, cryptox.IntegrationSecretAAD(item.integrationID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO integrations (id, code, client_id, client_secret_ciphertext,
				client_secret_key_version, redirect_uri)
			VALUES ($1, $2, $3, $4, $5, 'https://backend.example.test/oauth/amocrm/callback')`,
			item.integrationID, "widget-"+item.integrationID.String(), item.clientID, ciphertext, version); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO installations (id, integration_id, account_id, account_domain, status)
			VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active')`, item.installationID, item.integrationID); err != nil {
			t.Fatal(err)
		}
	}
	authenticator, err := NewAuthenticator(NewStore(pool), keys, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(clientID string, secret []byte, jti string) string {
		t.Helper()
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": "https://tenant.amocrm.ru", "aud": "https://backend.example.test",
			"jti": jti, "iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
			"exp": now.Add(10 * time.Minute).Unix(), "account_id": int64(42),
			"user_id": int64(84), "client_uuid": clientID,
		}).SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	assertPrincipal := func(item tenant, raw string) {
		t.Helper()
		principal, err := authenticator.Authenticate(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if principal.IntegrationID != item.integrationID || principal.InstallationID != item.installationID || principal.AccountID != 42 {
			t.Fatalf("incorrect tenant resolution: %+v", principal)
		}
	}
	// The same account, user, issuer, audience and jti must resolve separately
	// through each verified client_uuid and signing secret.
	sharedJTI := uuid.NewString()
	for _, item := range tenants {
		raw := sign(item.clientID, item.secret, sharedJTI)
		assertPrincipal(item, raw)
		if _, err := authenticator.Authenticate(ctx, raw); !errors.Is(err, ErrReplay) {
			t.Fatalf("same-integration replay error = %v", err)
		}
	}
	// Widget A cannot select B's installation by changing its client_uuid hint.
	forgedJTI := uuid.NewString()
	if _, err := authenticator.Authenticate(ctx, sign(tenants[1].clientID, tenants[0].secret, forgedJTI)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cross-integration signature error = %v", err)
	}
	assertPrincipal(tenants[1], sign(tenants[1].clientID, tenants[1].secret, forgedJTI))
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, tenants[0].integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.Authenticate(ctx, sign(tenants[0].clientID, tenants[0].secret, uuid.NewString())); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled integration authentication error = %v", err)
	}
	assertPrincipal(tenants[1], sign(tenants[1].clientID, tenants[1].secret, uuid.NewString()))
}
