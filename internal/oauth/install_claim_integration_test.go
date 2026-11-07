package oauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInstallDeniedCodeCanRetryAfterRefill(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	var exchanges atomic.Int32
	uncertain := errors.New("synthetic lost exchange response")
	gateway := &serviceCallbackGateway{exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
		exchanges.Add(1)
		return Token{}, uncertain
	}}
	service, err := NewService(store, keys, gateway, time.Minute, time.Second, InstallLimits{Rate: 0.2, Burst: 1, InactiveTTL: time.Minute, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "first", "fixture.amocrm.ru"); !errors.Is(err, uncertain) {
		t.Fatal(err)
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "retry", "fixture.amocrm.ru"); !errors.Is(err, errInstallCapacity) {
		t.Fatalf("rate denial=%v", err)
	}
	if claimed, err := store.HasInstallCodeClaim(ctx, "retry"); err != nil || claimed {
		t.Fatalf("denied claim=%v err=%v", claimed, err)
	}
	time.Sleep(5100 * time.Millisecond)
	if _, err := service.InstallCallback(ctx, integration.ClientID, "retry", "fixture.amocrm.ru"); !errors.Is(err, uncertain) {
		t.Fatalf("retry after real refill=%v", err)
	}
	if claimed, err := store.HasInstallCodeClaim(ctx, "retry"); err != nil || !claimed {
		t.Fatalf("admitted claim=%v err=%v", claimed, err)
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "retry", "fixture.amocrm.ru"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("uncertain exchange replay=%v", err)
	}
	if exchanges.Load() != 2 {
		t.Fatalf("exchanges=%d, want 2", exchanges.Load())
	}
}

type installCallbackCipher struct {
	Cipher
	beforeOpen func()
}

func (c installCallbackCipher) Open(version int, ciphertext, aad []byte) ([]byte, error) {
	c.beforeOpen()
	return c.Cipher.Open(version, ciphertext, aad)
}

func TestInstallConcurrentServicesClaimOnce(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	barrier := release
	cipher := installCallbackCipher{Cipher: keys, beforeOpen: func() { ready <- struct{}{}; <-barrier }}
	var exchanges atomic.Int32
	uncertain := errors.New("synthetic lost exchange response")
	gateway := &serviceCallbackGateway{exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
		exchanges.Add(1)
		return Token{}, uncertain
	}}
	results := make(chan error, 2)
	for range 2 {
		service, err := NewService(store, cipher, gateway, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, err := service.InstallCallback(ctx, integration.ClientID, "concurrent", "fixture.amocrm.ru")
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("both callbacks did not reach decryption after absent prechecks")
		}
	}
	close(release)
	release = nil
	var rejected, attempted int
	for range 2 {
		switch err := <-results; {
		case errors.Is(err, ErrInvalidState):
			rejected++
		case errors.Is(err, uncertain):
			attempted++
		default:
			t.Fatalf("concurrent callback=%v", err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oauth_states`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || attempted != 1 || rows != 1 || exchanges.Load() != 1 {
		t.Fatalf("rejected=%d attempted=%d rows=%d exchanges=%d", rejected, attempted, rows, exchanges.Load())
	}
}

func TestInstallCancellationBeforeClaim(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cipher := installCallbackCipher{Cipher: keys, beforeOpen: cancel}
	gateway := &serviceCallbackGateway{exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
		t.Error("cancelled callback reached exchange")
		return Token{}, nil
	}}
	service, err := NewService(store, cipher, gateway, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InstallCallback(ctx, integration.ClientID, "cancelled", "fixture.amocrm.ru"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled callback=%v", err)
	}
	if claimed, err := store.HasInstallCodeClaim(context.Background(), "cancelled"); err != nil || claimed {
		t.Fatalf("cancelled claim=%v err=%v", claimed, err)
	}
}

func TestStoreInstallClaimLookupMatchesUniqueConstraint(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	other, err := store.EnsureIntegration(ctx, IntegrationInput{Code: "other", ClientID: uuid.NewString(), ClientSecret: "synthetic", RedirectURI: "https://example.test/oauth", WebhookEvents: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.HasInstallCodeClaim(ctx, "absent"); err != nil || claimed {
		t.Fatalf("absent=%v err=%v", claimed, err)
	}
	for _, tt := range []struct {
		code string
		id   uuid.UUID
		ttl  time.Duration
	}{
		{"present", integration.ID, time.Minute},
		{"expired", integration.ID, time.Minute},
		{"other-integration", other.ID, time.Minute},
	} {
		if err := store.ClaimInstallCode(ctx, tt.id, tt.code, tt.ttl); err != nil {
			t.Fatal(err)
		}
		if tt.code == "expired" {
			hash := installCodeHash(tt.code)
			if _, err := pool.Exec(ctx, `UPDATE oauth_states SET created_at=now()-interval '3 minutes', consumed_at=now()-interval '2 minutes', expires_at=now()-interval '1 minute' WHERE state_hash=$1`, hash[:]); err != nil {
				t.Fatal(err)
			}
		}
		if claimed, err := store.HasInstallCodeClaim(ctx, tt.code); err != nil || !claimed {
			t.Fatalf("%s=%v err=%v", tt.code, claimed, err)
		}
		if err := store.ClaimInstallCode(ctx, integration.ID, tt.code, installCodeTTL); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%s uniqueness=%v", tt.code, err)
		}
	}
	hash := sha256.Sum256([]byte("amocrm-install-code\x00ordinary-state"))
	if _, err := pool.Exec(ctx, `INSERT INTO oauth_states (integration_id, state_hash, metadata, expires_at) VALUES ($1, $2, '{}', now() + interval '1 minute')`, other.ID, hash[:]); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.HasInstallCodeClaim(ctx, "ordinary-state"); err != nil || !claimed {
		t.Fatalf("ordinary state=%v err=%v", claimed, err)
	}
	if err := store.ClaimInstallCode(ctx, integration.ID, "ordinary-state", installCodeTTL); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ordinary state uniqueness=%v", err)
	}
	closedPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	closedPool.Close()
	if claimed, err := NewStore(closedPool, keys).HasInstallCodeClaim(ctx, "absent"); err == nil || claimed {
		t.Fatalf("failed lookup treated as absence: claimed=%v err=%v", claimed, err)
	}
}

func TestStoreClaimsInstallCodeOnceConcurrently(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	store := NewStore(pool, oauthTestKeyRing(t))
	integration := oauthTestIntegration(t, store)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- store.ClaimInstallCode(context.Background(), integration.ID, "direct-race", installCodeTTL)
		}()
	}
	close(start)
	var successes, replays int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrInvalidState):
			replays++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("direct claims successes=%d replays=%d", successes, replays)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_states`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("direct claim rows=%d, want 1", rows)
	}
}
