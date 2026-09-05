package oauth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOAuthIsolatesIntegrationsInSameAccount(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	ctx := context.Background()
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	inputs := []IntegrationInput{
		{Code: "widget-alpha", ClientID: uuid.NewString(), ClientSecret: "synthetic-alpha-secret", RedirectURI: "https://alpha.example.test/oauth/callback", WebhookEvents: []string{"update_lead"}},
		{Code: "widget-beta", ClientID: uuid.NewString(), ClientSecret: "synthetic-beta-secret", RedirectURI: "https://beta.example.test/oauth/callback", WebhookEvents: []string{"add_contact"}},
	}
	integrations := make([]Integration, len(inputs))
	for i, input := range inputs {
		var err error
		integrations[i], err = store.EnsureIntegration(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	exchanges := 0
	gateway := &serviceCallbackGateway{
		exchangeCode: func(_ context.Context, domain, clientID, secret, redirect, code string) (Token, error) {
			exchanges++
			for _, input := range inputs {
				if input.ClientID != clientID {
					continue
				}
				if domain != "shared.amocrm.ru" || secret != input.ClientSecret || redirect != input.RedirectURI || code != input.Code {
					t.Fatalf("OAuth state selected incorrect credentials or redirect for %s", input.Code)
				}
				return Token{AccessToken: "access-" + input.Code, RefreshToken: "refresh-" + input.Code, ExpiresIn: 3600}, nil
			}
			t.Fatal("OAuth state selected unknown integration")
			return Token{}, errors.New("unknown integration")
		},
		getAccount: func(_ context.Context, domain, accessToken string) (Account, error) {
			if domain != "shared.amocrm.ru" || (accessToken != "access-widget-alpha" && accessToken != "access-widget-beta") {
				t.Fatal("unexpected account lookup")
			}
			return Account{ID: 42, Subdomain: "shared"}, nil
		},
	}
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)
	start := func(input IntegrationInput) string {
		t.Helper()
		authorizeURL, err := service.Start(ctx, input.Code, "/widget/"+input.Code)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Query().Get("client_id") != input.ClientID || parsed.Query().Get("state") == "" {
			t.Fatal("OAuth start selected incorrect client or empty state")
		}
		return parsed.Query().Get("state")
	}
	states := []string{start(inputs[0]), start(inputs[1])}
	results := make([]InstallationResult, 2)
	// Callback order deliberately differs from start order; no process-global
	// current integration may affect the chosen secret, redirect or installation.
	for _, i := range []int{1, 0} {
		result, err := service.Callback(ctx, states[i], inputs[i].Code, "https://shared.amocrm.ru")
		if err != nil {
			t.Fatal(err)
		}
		results[i] = result
		if result.ID == uuid.Nil || result.AccountID != 42 || result.Status != "active" {
			t.Fatalf("incorrect callback result: %+v", result)
		}
	}
	if results[0].ID == results[1].ID {
		t.Fatal("two integrations shared an installation")
	}
	for i, result := range results {
		var integrationID uuid.UUID
		var accessCiphertext, refreshCiphertext []byte
		var version int
		if err := pool.QueryRow(ctx, `
			SELECT i.integration_id, c.access_token_ciphertext, c.refresh_token_ciphertext, c.key_version
			FROM installations i JOIN oauth_credentials c ON c.installation_id=i.id
			WHERE i.id=$1`, result.ID).Scan(&integrationID, &accessCiphertext, &refreshCiphertext, &version); err != nil {
			t.Fatal(err)
		}
		if integrationID != integrations[i].ID {
			t.Fatal("credentials stored under another integration")
		}
		access, err := keys.Open(version, accessCiphertext, credentialsAAD(result.ID))
		if err != nil {
			t.Fatal(err)
		}
		refresh, err := keys.Open(version, refreshCiphertext, credentialsAAD(result.ID))
		if err != nil {
			t.Fatal(err)
		}
		if string(access) != "access-"+inputs[i].Code || string(refresh) != "refresh-"+inputs[i].Code {
			t.Fatal("installation received another integration's credentials")
		}
		if _, err := keys.Open(version, accessCiphertext, credentialsAAD(results[1-i].ID)); err == nil {
			t.Fatal("credentials accepted another installation's AAD")
		}
		if _, err := service.Callback(ctx, states[i], inputs[i].Code, "https://shared.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("state replay error = %v", err)
		}
	}
	if exchanges != 2 {
		t.Fatalf("token exchange count = %d, want 2", exchanges)
	}
	// Disabling A invalidates its outstanding state and start without affecting
	// an already issued state for B or B's existing installation.
	states = []string{start(inputs[0]), start(inputs[1])}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, integrations[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(ctx, inputs[0].Code, "/widget"); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("disabled integration start error = %v", err)
	}
	if _, err := service.Callback(ctx, states[0], inputs[0].Code, "https://shared.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("disabled integration callback error = %v", err)
	}
	result, err := service.Callback(ctx, states[1], inputs[1].Code, "https://shared.amocrm.ru")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != results[1].ID || exchanges != 3 {
		t.Fatalf("unaffected integration callback result = %+v, exchanges = %d", result, exchanges)
	}
}
