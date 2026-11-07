package oauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInstallCapacityRejectsBeforeClaimAndKeepsStateRecovery(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	other, err := store.EnsureIntegration(ctx, IntegrationInput{Code: "other", ClientID: uuid.NewString(), ClientSecret: "synthetic-secret", RedirectURI: "https://example.test/oauth", WebhookEvents: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	upstream := errors.New("synthetic exchange failure")
	gateway := &serviceCallbackGateway{exchangeCode: func(_ context.Context, _, _, _, _, code string) (Token, error) {
		if strings.HasPrefix(code, "held-") {
			started <- struct{}{}
			<-release
		}
		return Token{}, upstream
	}}
	service, err := NewService(store, keys, gateway, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := service.InstallCallback(ctx, integration.ClientID, fmt.Sprintf("held-%d", i), "fixture.amocrm.ru")
			finished <- err
		}()
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("exchange did not start")
		}
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "no-capacity", "fixture.amocrm.ru"); !errors.Is(err, errInstallCapacity) {
		t.Fatalf("capacity rejection=%v", err)
	}
	if err := store.ClaimInstallCode(ctx, integration.ID, "no-capacity", installCodeTTL); err != nil {
		t.Fatalf("capacity denial consumed code: %v", err)
	}
	state, _, err := store.CreateState(ctx, integration.ID, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Callback(ctx, state, "recovery", "fixture.amocrm.ru"); !errors.Is(err, upstream) {
		t.Fatalf("state recovery blocked: %v", err)
	}
	if _, err := service.InstallCallback(ctx, other.ClientID, "other-code", "fixture.amocrm.ru"); !errors.Is(err, upstream) {
		t.Fatalf("other integration blocked: %v", err)
	}
	unblock()
	for range 8 {
		if err := <-finished; !errors.Is(err, upstream) {
			t.Fatalf("held exchange=%v", err)
		}
	}
}

func TestMalformedCallbackPreservesUnusedState(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	service, err := NewService(store, keys, &serviceCallbackGateway{}, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ code, referer string }{{"", "fixture.amocrm.ru"}, {"code", "evil.example.test"}} {
		state, _, err := store.CreateState(context.Background(), integration.ID, "", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Callback(context.Background(), state, tt.code, tt.referer); !errors.Is(err, ErrInvalidState) {
			t.Errorf("malformed callback=%v", err)
		}
		if _, err := store.ConsumeState(context.Background(), state); err != nil {
			t.Errorf("malformed callback consumed state: %v", err)
		}
	}
}

func TestInstallCallbackReplayDoesNotSpendExchangeBurst(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	if err := store.ClaimInstallCode(ctx, integration.ID, "already-claimed", installCodeTTL); err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	upstream := errors.New("synthetic exchange failure")
	gateway := &serviceCallbackGateway{exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
		exchanges.Add(1)
		return Token{}, upstream
	}}
	service, serviceErr := NewService(store, keys, gateway, time.Minute, 5*time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: 10 * time.Minute, MaxEntries: 10_000})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	for range 20 {
		if _, err := service.InstallCallback(ctx, integration.ClientID, "already-claimed", "fixture.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("replay = %v", err)
		}
	}
	for i := range 8 {
		if _, err := service.InstallCallback(ctx, integration.ClientID, fmt.Sprintf("new-%d", i), "fixture.amocrm.ru"); !errors.Is(err, upstream) {
			t.Fatalf("untouched burst %d = %v", i, err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := service.InstallCallback(bounded, integration.ClientID, "ninth", "fixture.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("post-claim rate wait = %v, want permanent restart", err)
	}
	if exchanges.Load() != 8 {
		t.Errorf("exchange attempts = %d, want 8", exchanges.Load())
	}
	if err := store.ClaimInstallCode(ctx, integration.ID, "ninth", installCodeTTL); !errors.Is(err, ErrInvalidState) {
		t.Errorf("unsent code claim reopened: %v", err)
	}
}

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
	service, serviceErr := NewService(store, keys, gateway, time.Minute, 5*time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: 10 * time.Minute, MaxEntries: 10_000})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}

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
	service, serviceErr := NewService(store, keys, gateway, time.Minute, 5*time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: 10 * time.Minute, MaxEntries: 10_000})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}

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
