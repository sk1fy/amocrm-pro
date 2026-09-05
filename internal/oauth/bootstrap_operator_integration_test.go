package oauth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestBootstrapPreservesOperatorChangesAndDisabledIntegration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	original := oauthTestIntegration(t, store)
	operator := integrations.NewStore(pool, keys)
	for _, c := range []integrations.Command{
		{Action: "rotate-secret", Actor: "test-operator", Code: original.Code, Secret: []byte("operator-rotated-secret")},
		{Action: "disable", Actor: "test-operator", Code: original.Code},
		{Action: "set-service", Actor: "test-operator", Code: original.Code, Service: "lead-status", Enabled: false},
	} {
		if _, err := operator.Apply(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	existing, err := store.EnsureIntegration(ctx, IntegrationInput{Code: original.Code, ClientID: original.ClientID, ClientSecret: "stale-env-secret", RedirectURI: "https://example.test/stale", WebhookEvents: []string{"stale_event"}})
	if err != nil {
		t.Fatal(err)
	}
	if existing.ID != original.ID || existing.RedirectURI != original.RedirectURI || bytes.Equal(existing.ClientSecretCiphertext, original.ClientSecretCiphertext) {
		t.Fatal("bootstrap overwrote operator configuration")
	}
	plain, err := keys.Open(existing.ClientSecretKeyVersion, existing.ClientSecretCiphertext, integrationSecretAAD(existing.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "operator-rotated-secret" {
		t.Fatal("bootstrap overwrote rotated secret")
	}
	if _, err := store.FindIntegrationByCode(ctx, existing.Code); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("bootstrap reactivated integration: %v", err)
	}
	var enabled bool
	var audits int
	if err := pool.QueryRow(ctx, `SELECT enabled,(SELECT count(*) FROM audit_log WHERE actor_type='bootstrap') FROM integration_services WHERE integration_id=$1 AND service_code='lead-status'`, existing.ID).Scan(&enabled, &audits); err != nil {
		t.Fatal(err)
	}
	if enabled || audits != 1 {
		t.Fatal("bootstrap granted service again or audited an unchanged row")
	}
}

func TestSaveInstallationRejectsIntegrationDisabledAfterStateConsumption(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	rawState, _, err := store.CreateState(ctx, integration.ID, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ConsumeState(ctx, rawState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := integrations.NewStore(pool, keys).Apply(ctx, integrations.Command{Action: "disable", Actor: "test-operator", Code: integration.Code}); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveInstallation(ctx, state.Integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600})
	if !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("authorization after disable: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM installations)+(SELECT count(*) FROM oauth_credentials)+(SELECT count(*) FROM jobs)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("disabled authorization persisted side effects")
	}
}
