package oauth

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiredLeaseDoesNotInvalidateSuccessfulRotation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installation := oauthTestInstallation(t, pool, keys, -1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var calls atomic.Int32
	gateway := &oauthTestGateway{refresh: func(ctx context.Context, _, _, _, _, refresh string) (Token, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return Token{}, ctx.Err()
			}
			return Token{AccessToken: "valid-new-access", RefreshToken: "valid-new-refresh", ExpiresIn: 3600}, nil
		}
		return Token{}, &amocrm.APIError{Kind: amocrm.ErrorUnauthorized}
	}}
	first, second := NewTokenProvider(pool, keys, gateway), NewTokenProvider(pool, keys, gateway)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := first.Token(ctx, installation); firstDone <- err }()
	<-entered
	// The remote token has been consumed, but its successful response is delayed
	// past the lease (e.g. paused process / network / local finalization outage).
	if _, err := pool.Exec(ctx, `UPDATE oauth_credentials SET lease_until=now()-interval '1 second' WHERE installation_id=$1`, installation); err != nil {
		t.Fatal(err)
	}
	_, secondErr := second.Token(ctx, installation)
	if !errors.Is(secondErr, ErrRefreshOutcomeUnknown) {
		t.Fatalf("expected unknown outcome, got %v", secondErr)
	}
	if calls.Load() != 1 {
		t.Fatal("one-time refresh repeated")
	}
	once.Do(func() { close(release) })
	if err := <-firstDone; err != nil {
		t.Fatalf("original successful rotation failed: %v", err)
	}
	var state string
	var version int64
	if err := pool.QueryRow(ctx, `SELECT i.status,c.token_version FROM installations i JOIN oauth_credentials c ON c.installation_id=i.id WHERE i.id=$1`, installation).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("successful rotation left installation %s with credential version=%d after %d refresh calls", state, version, calls.Load())
	}
}

func TestUncertainRefreshSurvivesProviderRestartAndReauthorization(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	id := oauthTestInstallation(t, pool, keys, -1)
	ctx := context.Background()
	var calls atomic.Int32
	gw := &oauthTestGateway{refresh: func(context.Context, string, string, string, string, string) (Token, error) {
		calls.Add(1)
		return Token{}, context.DeadlineExceeded
	}}
	first := NewTokenProvider(pool, keys, gw)
	if _, err := first.Token(ctx, id); err == nil {
		t.Fatal("expected unknown remote result")
	}
	if _, err := pool.Exec(ctx, `UPDATE oauth_credentials SET lease_until=now()-interval '1 second' WHERE installation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	restarted := NewTokenProvider(pool, keys, gw)
	if _, err := restarted.Token(ctx, id); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("restart must preserve uncertainty: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("restart resent consumed refresh")
	}
	// An explicit reauthorization resolves the generation and drops its lease.
	store := NewStore(pool, keys)
	var integrationID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT integration_id FROM installations WHERE id=$1`, id).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	var integration Integration
	// SaveInstallation uses ID/webhook settings; client credentials remain in DB.
	integration.ID = integrationID
	integration.WebhookEvents = []string{}
	if _, err := store.SaveInstallation(ctx, integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{AccessToken: "reauthorized", RefreshToken: "reauthorized-refresh", ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Token(ctx, id)
	if err != nil || got.Value != "reauthorized" {
		t.Fatalf("reauthorization did not resolve claim: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("reauthorization caused another refresh")
	}
}

func TestRotatedTokenFinalizationRequiresItsLease(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	id := oauthTestInstallation(t, pool, keys, -1)
	p := NewTokenProvider(pool, keys, nil)
	ctx := context.Background()
	claim, _, _, err := p.captureRefresh(ctx, id, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil {
		t.Fatal("missing claim")
	}
	newLease := uuid.New()
	if _, err := pool.Exec(ctx, `UPDATE oauth_credentials SET lease_token=$2 WHERE installation_id=$1`, id, newLease); err != nil {
		t.Fatal(err)
	}
	_, err = p.finalizeRotated(ctx, id, pendingRotation{observedVersion: 1, leaseToken: claim.leaseToken, token: Token{AccessToken: "stale", RefreshToken: "stale", ExpiresIn: 3600}})
	if !errors.Is(err, errRefreshFenceLost) {
		t.Fatalf("stale lease finalized: %v", err)
	}
	assertStoredRefresh(t, pool, keys, id, "refresh-initial", 1)
}
