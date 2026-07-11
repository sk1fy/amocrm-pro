package oauth

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestSaveInstallationReauthorizationRefreshesWebhookIntent(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	integration := oauthTestIntegration(t, store)
	initial, err := store.SaveInstallation(ctx, integration, Account{
		ID: 42, Subdomain: "tenant",
	}, "tenant.amocrm.ru", Token{
		AccessToken: "access-initial", RefreshToken: "refresh-initial", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE installations
		SET webhook_status = 'active', webhook_settings = '["old_event"]'::jsonb
		WHERE id = $1`, initial.ID); err != nil {
		t.Fatal(err)
	}

	wantEvents := []string{"add_lead", "update_contact"}
	updatedIntegration, err := store.EnsureIntegration(ctx, IntegrationInput{
		Code:          integration.Code,
		ClientID:      integration.ClientID,
		ClientSecret:  "synthetic-client-secret-rotated",
		RedirectURI:   integration.RedirectURI,
		WebhookEvents: wantEvents,
	})
	if err != nil {
		t.Fatal(err)
	}
	reauthorized, err := store.SaveInstallation(ctx, updatedIntegration, Account{
		ID: 42, Subdomain: "tenant",
	}, "tenant.amocrm.ru", Token{
		AccessToken: "access-reauthorized", RefreshToken: "refresh-reauthorized", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reauthorized.ID != initial.ID {
		t.Fatalf("installation id changed during reauthorization: got %s, want %s", reauthorized.ID, initial.ID)
	}

	var installationStatus, webhookStatus string
	var settingsJSON json.RawMessage
	var tokenVersion int64
	if err := pool.QueryRow(ctx, `
		SELECT installation.status, installation.webhook_status,
			installation.webhook_settings, credentials.token_version
		FROM installations installation
		JOIN oauth_credentials credentials ON credentials.installation_id = installation.id
		WHERE installation.id = $1`, initial.ID,
	).Scan(&installationStatus, &webhookStatus, &settingsJSON, &tokenVersion); err != nil {
		t.Fatal(err)
	}
	var gotEvents []string
	if err := json.Unmarshal(settingsJSON, &gotEvents); err != nil {
		t.Fatal(err)
	}
	if installationStatus != "active" || webhookStatus != "pending" {
		t.Fatalf("reauthorization state: installation=%s webhook=%s", installationStatus, webhookStatus)
	}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Fatalf("webhook settings = %#v, want %#v", gotEvents, wantEvents)
	}
	if tokenVersion != 2 {
		t.Fatalf("token version = %d, want 2", tokenVersion)
	}

	var reconcileJobs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE installation_id = $1 AND type = 'webhook.reconcile'`, initial.ID,
	).Scan(&reconcileJobs); err != nil {
		t.Fatal(err)
	}
	if reconcileJobs != 2 {
		t.Fatalf("reconcile jobs = %d, want 2", reconcileJobs)
	}
}
