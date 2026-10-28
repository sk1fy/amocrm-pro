package oauth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInstallCallbackReactivatesExistingInstallationOnce(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()

	var exchanges atomic.Int32
	var redirectURI string
	gateway := &serviceCallbackGateway{
		exchangeCode: func(_ context.Context, _, clientID, _, redirect, _ string) (Token, error) {
			exchanges.Add(1)
			if clientID != integration.ClientID {
				t.Fatalf("exchange client id = %q", clientID)
			}
			redirectURI = redirect
			return Token{AccessToken: "install-access", RefreshToken: "install-refresh", ExpiresIn: 3600}, nil
		},
		getAccount: func(_ context.Context, domain, _ string) (Account, error) {
			if domain != "reinstall.amocrm.ru" {
				t.Fatalf("account domain = %q", domain)
			}
			return Account{ID: 31391206, Subdomain: "reinstall"}, nil
		},
	}
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)

	first, err := service.InstallCallback(ctx, integration.ClientID, "install-code-1", "reinstall.amocrm.ru")
	if err != nil {
		t.Fatal(err)
	}
	// Disabling the widget revokes its tokens; the refresh path then parks the
	// installation until amoCRM authorizes it again.
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='reauth_required' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := service.InstallCallback(ctx, integration.ClientID, "install-code-2", "https://reinstall.amocrm.ru")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Status != "active" || redirectURI != integration.RedirectURI {
		t.Fatalf("reinstall result %#v first=%s redirect=%q", second, first.ID, redirectURI)
	}
	var status string
	var tokenVersion, installations int
	if err := pool.QueryRow(ctx, `SELECT i.status, c.token_version, (SELECT count(*) FROM installations) FROM installations i JOIN oauth_credentials c ON c.installation_id=i.id WHERE i.id=$1`, first.ID).Scan(&status, &tokenVersion, &installations); err != nil {
		t.Fatal(err)
	}
	if status != "active" || tokenVersion != 2 || installations != 1 {
		t.Fatalf("installation status=%s token_version=%d installations=%d", status, tokenVersion, installations)
	}

	if _, err := service.InstallCallback(ctx, integration.ClientID, "install-code-2", "reinstall.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("replayed install code error = %v", err)
	}
	// An install code claim is never usable as an OAuth state.
	if _, err := service.Callback(ctx, "install-code-2", "x", "reinstall.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("install code as state error = %v", err)
	}
	if exchanges.Load() != 2 {
		t.Fatalf("code exchanges = %d, want 2", exchanges.Load())
	}
}

func TestInstallCallbackRejectsUnknownOrDisabledIntegrationBeforeExchange(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	gateway := &serviceCallbackGateway{
		exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
			t.Fatal("rejected install callback reached amoCRM")
			return Token{}, nil
		},
	}
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)

	for name, call := range map[string][3]string{
		"unknown client":  {uuid.NewString(), "code", "acc.amocrm.ru"},
		"malformed":       {"not-a-uuid", "code", "acc.amocrm.ru"},
		"missing code":    {integration.ClientID, " ", "acc.amocrm.ru"},
		"foreign referer": {integration.ClientID, "code", "evil.example.com"},
	} {
		if _, err := service.InstallCallback(ctx, call[0], call[1], call[2]); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, integration.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "code", "acc.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("disabled integration error = %v", err)
	}
}
