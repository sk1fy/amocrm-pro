package webhook

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"net/http"
	"strings"
	"testing"
)

func TestReconcilePreservesForeignWebhooks(t *testing.T) {
	for _, unregister := range []bool{false, true} {
		name := "reconcile"
		if unregister {
			name = "uninstall"
		}
		t.Run(name, func(t *testing.T) {
			desired := "https://ours.example.test/hooks/amocrm/v1/current"
			foreign := "https://other-crm-connector.example.test/events"
			gw := &fakeWebhookGateway{listed: []amocrm.Webhook{{Destination: desired, Settings: []string{"add_lead"}}, {Destination: foreign, Settings: []string{"add_lead"}}}}
			if err := syncInstallationWebhooks(context.Background(), gw, uuid.New(), desired, []string{"add_lead"}, unregister, desired); err != nil {
				t.Fatal(err)
			}
			for _, destination := range gw.deleted {
				if destination == foreign {
					t.Fatalf("%s deleted another integration's webhook: %s", name, destination)
				}
			}
		})
	}
}

func TestOwnershipRegistrySurvivesKeyRotationAndPreservesSameHostNeighbor(t *testing.T) {
	pool := testkit.Postgres(t)
	f := newReconcileFixture(t, pool)
	ctx := context.Background()
	old := reconcilePublicBaseURL + "/hooks/amocrm/v1/previous-secret"
	neighbor := reconcilePublicBaseURL + "/hooks/amocrm/v1/another-installation-secret"
	if err := f.store.rememberDestination(ctx, f.installationID, old); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT destination_ciphertext FROM installation_webhook_destinations WHERE installation_id=$1`, f.installationID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "previous-secret") {
		t.Fatal("plaintext webhook URL persisted")
	}
	gw := &fakeWebhookGateway{listed: []amocrm.Webhook{{Destination: old, Settings: f.settings}, {Destination: f.destination(), Settings: f.settings}, {Destination: neighbor, Settings: f.settings}}}
	handler, err := ReconcileJobHandler(NewReconcileStore(pool, f.store.keys), gw, reconcilePublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler(ctx, f.job(f.installationID)); err != nil {
		t.Fatal(err)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != old {
		t.Fatalf("wrong ownership deletion: %+v", gw.deleted)
	}
	owned, err := f.store.ownedDestinations(ctx, f.installationID)
	if err != nil || len(owned) != 1 || owned[0] != f.destination() {
		t.Fatalf("registry not compacted after success: %+v %v", owned, err)
	}
}

func TestLegacyWebhookOwnershipRequiresExactSecretPath(t *testing.T) {
	for _, candidate := range []string{"https://host.test/hooks/amocrm/v1/other", "https://host.test/hooks/amocrm/v1/current/extra", "https://host.test/hooks/amocrm/v1/current?x=1", "https://user@host.test/hooks/amocrm/v1/current"} {
		if hasInstallationWebhookKey(candidate, "current") {
			t.Fatalf("unproven ownership: %s", candidate)
		}
	}
	if !hasInstallationWebhookKey("https://old-host.test/hooks/amocrm/v1/current", "current") {
		t.Fatal("exact legacy key not recognized")
	}
}

func TestWebhookOwnershipIntentSurvivesLostRegistrationResponse(t *testing.T) {
	pool := testkit.Postgres(t)
	f := newReconcileFixture(t, pool)
	ctx := context.Background()
	gw := &fakeWebhookGateway{registerErr: errors.New("response lost")}
	handler, err := ReconcileJobHandler(f.store, gw, reconcilePublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler(ctx, f.job(f.installationID)); err == nil {
		t.Fatal("expected lost response")
	}
	owned, err := NewReconcileStore(pool, f.store.keys).ownedDestinations(ctx, f.installationID)
	if err != nil || len(owned) != 1 || owned[0] != f.destination() {
		t.Fatalf("registration intent was not durable: %+v %v", owned, err)
	}
	// The provider applied the request. A restarted worker observes it and
	// converges without deleting it or a same-host neighbor.
	gw.registerErr = nil
	gw.listed = []amocrm.Webhook{{Destination: f.destination(), Settings: f.settings}, {Destination: reconcilePublicBaseURL + "/hooks/amocrm/v1/foreign", Settings: f.settings}}
	if _, err := handler(ctx, f.job(f.installationID)); err != nil {
		t.Fatal(err)
	}
	if len(gw.registered) != 0 || len(gw.deleted) != 0 {
		t.Fatal("recovery changed already converged hooks")
	}
}

func TestUninstallCanRecoverExpiredAccessToken(t *testing.T) {
	pool := testkit.Postgres(t)
	f := newReconcileFixture(t, pool)
	ctx := context.Background()
	access, version, err := f.store.keys.Seal([]byte("expired-access"), cryptox.InstallationOAuthAAD(f.installationID))
	if err != nil {
		t.Fatal(err)
	}
	refresh, _, err := f.store.keys.Seal([]byte("valid-refresh"), cryptox.InstallationOAuthAAD(f.installationID))
	if err != nil {
		t.Fatal(err)
	}
	secret, secretVersion, err := f.store.keys.Seal([]byte("valid-client-secret"), cryptox.IntegrationSecretAAD(f.integrationID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET client_secret_ciphertext=$2,client_secret_key_version=$3 WHERE id=$1`, f.integrationID, secret, secretVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oauth_credentials(installation_id,access_token_ciphertext,refresh_token_ciphertext,expires_at,key_version) VALUES($1,$2,$3,now()-interval '1 hour',$4)`, f.installationID, access, refresh, version); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='uninstalled' WHERE id=$1`, f.installationID); err != nil {
		t.Fatal(err)
	}
	requests, deletes := 0, 0
	httpClient := &http.Client{Transport: reconcileRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path == "/oauth2/access_token" {
			return reconcileResponse(200, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`, nil), nil
		}
		if r.Header.Get("Authorization") == "Bearer expired-access" {
			return reconcileResponse(401, `{}`, nil), nil
		}
		if r.Method == http.MethodDelete {
			deletes++
			return reconcileResponse(204, "", nil), nil
		}
		if deletes > 0 {
			return reconcileResponse(200, `{"_embedded":{"webhooks":[]}}`, nil), nil
		}
		return reconcileResponse(200, `{"_embedded":{"webhooks":[{"destination":"`+f.destination()+`"}]}}`, nil), nil
	})}
	tokens, err := oauth.NewUninstallTokenProvider(pool, f.store.keys, oauth.NewGateway(amocrm.NewOAuthClient(httpClient)), f.installationID)
	if err != nil {
		t.Fatal(err)
	}
	client := amocrm.NewClient(httpClient, tokens)
	// Follow the CLI's own advice to rerun uninstall once.
	_ = Unregister(ctx, f.store, client, f.installationID)
	err = Unregister(ctx, f.store, client, f.installationID)
	if err != nil || deletes == 0 {
		t.Fatalf("retry cannot unregister with expired access and valid refresh: requests=%d deletes=%d err=%v", requests, deletes, err)
	}
	if _, err := tokens.Token(ctx, uuid.New()); err == nil {
		t.Fatal("cleanup provider accepted another installation")
	}
	normal := oauth.NewTokenProvider(pool, f.store.keys, oauth.NewGateway(amocrm.NewOAuthClient(httpClient)))
	if _, err := normal.Token(ctx, f.installationID); err == nil {
		t.Fatal("product token access reopened by cleanup")
	}
}
