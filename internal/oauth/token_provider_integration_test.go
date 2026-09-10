package oauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestTokenProviderCoordinatesConcurrentRefreshAcrossProviders(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)

	var refreshCalls atomic.Int32
	gateway := &oauthTestGateway{
		refresh: func(_ context.Context, _, _, _, _, refreshToken string) (Token, error) {
			refreshCalls.Add(1)
			time.Sleep(75 * time.Millisecond)
			if refreshToken != "refresh-initial" {
				return Token{}, errors.New("unexpected refresh token")
			}
			return Token{AccessToken: "access-rotated", RefreshToken: "refresh-rotated", ExpiresIn: 3600}, nil
		},
	}
	providers := []*TokenProvider{
		NewTokenProvider(pool, keys, gateway),
		NewTokenProvider(pool, keys, gateway),
	}

	const callers = 8
	start := make(chan struct{})
	results := make(chan amocrm.AccessToken, callers)
	errorsByCaller := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for i := range callers {
		go func() {
			defer group.Done()
			<-start
			token, err := providers[i%len(providers)].Token(context.Background(), installationID)
			results <- token
			errorsByCaller <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsByCaller)

	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("concurrent token refresh: %v", err)
		}
	}
	for token := range results {
		if token.Value != "access-rotated" || token.TokenVersion != 2 {
			t.Fatalf("unexpected refreshed token: %#v", token)
		}
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
}

func TestTokenProviderDoesNotReuseExpiredRefreshLease(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	t.Cleanup(func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) })
	var refreshCalls atomic.Int32
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(_ context.Context, _, _, _, _, refreshToken string) (Token, error) {
			call := refreshCalls.Add(1)
			if call == 1 {
				close(firstEntered)
				<-releaseFirst
				return Token{AccessToken: "access-from-first", RefreshToken: "refresh-from-first", ExpiresIn: 3600}, nil
			}
			if refreshToken != "refresh-initial" {
				return Token{}, errors.New("stolen refresh used a different token")
			}
			return Token{AccessToken: "access-from-second", RefreshToken: "refresh-from-second", ExpiresIn: 3600}, nil
		},
	})
	provider.leaseTTL = 40 * time.Millisecond
	provider.leasePoll = 5 * time.Millisecond

	firstError := make(chan error, 1)
	firstToken := make(chan amocrm.AccessToken, 1)
	go func() {
		token, err := provider.Token(context.Background(), installationID)
		firstToken <- token
		firstError <- err
	}()
	<-firstEntered
	time.Sleep(120 * time.Millisecond)

	_, err := provider.Token(context.Background(), installationID)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("expired lease must be unknown, got %v", err)
	}
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	if err := <-firstError; err != nil {
		t.Fatal(err)
	}
	if token := <-firstToken; token.Value != "access-from-first" || token.TokenVersion != 2 {
		t.Fatalf("late owner failed: %#v", token)
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("consumed refresh repeated: %d calls", calls)
	}
	assertStoredRefresh(t, pool, keys, installationID, "refresh-from-first", 2)
}

func TestTokenProviderForcedRefreshUsesObservedVersion(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)

	var refreshCalls atomic.Int32
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			refreshCalls.Add(1)
			return Token{AccessToken: "access-rotated", RefreshToken: "refresh-rotated", ExpiresIn: 3600}, nil
		},
	})
	rotated, err := provider.Token(context.Background(), installationID)
	if err != nil {
		t.Fatal(err)
	}
	stale := amocrm.AccessToken{
		InstallationID: installationID,
		TokenVersion:   1,
		Value:          "access-initial",
	}
	current, err := provider.RefreshIfCurrent(context.Background(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if current.Value != rotated.Value || current.TokenVersion != 2 {
		t.Fatalf("forced refresh ignored stored version: %#v", current)
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
}

func TestMarkReauthRequiredSkipsLiveRefreshLease(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, 3600)
	if _, err := pool.Exec(context.Background(), `
		UPDATE oauth_credentials
		SET lease_token = $2, lease_until = now() + interval '1 minute'
		WHERE installation_id = $1`, installationID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	provider := NewTokenProvider(pool, keys, nil)
	if err := provider.MarkReauthRequired(context.Background(), installationID, 1); err != nil {
		t.Fatal(err)
	}
	assertOAuthInstallationStatus(t, pool, installationID, "active")
}

func TestTokenProviderPersistsPendingRotationWithoutSecondRefresh(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE SEQUENCE oauth_test_finalize_attempts;
		CREATE FUNCTION oauth_test_fail_finalize() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.token_version IS DISTINCT FROM OLD.token_version THEN
				IF nextval('oauth_test_finalize_attempts') <= %d THEN
					RAISE EXCEPTION 'synthetic token persist failure';
				END IF;
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER oauth_test_fail_finalize
		BEFORE UPDATE ON oauth_credentials
		FOR EACH ROW EXECUTE FUNCTION oauth_test_fail_finalize()`, tokenPersistAttempts)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupContext, `
			DROP TRIGGER IF EXISTS oauth_test_fail_finalize ON oauth_credentials;
			DROP FUNCTION IF EXISTS oauth_test_fail_finalize();
			DROP SEQUENCE IF EXISTS oauth_test_finalize_attempts`)
	})

	var refreshCalls atomic.Int32
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			refreshCalls.Add(1)
			return Token{AccessToken: "access-pending", RefreshToken: "refresh-pending", ExpiresIn: 3600}, nil
		},
	})
	if _, err := provider.Token(ctx, installationID); err == nil {
		t.Fatal("expected local persist failure after remote refresh")
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
	access, err := provider.Token(ctx, installationID)
	if err != nil {
		t.Fatal(err)
	}
	if access.Value != "access-pending" || access.TokenVersion != 2 {
		t.Fatalf("unexpected recovered token: %#v", access)
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("pending persist repeated refresh: calls=%d", calls)
	}
	assertStoredRefresh(t, pool, keys, installationID, "refresh-pending", 2)
}

func TestTokenProviderRetriesPersistAfterEncryptionKeyChange(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)
	cipher := &versionFlipCipher{inner: keys}
	var refreshCalls atomic.Int32
	provider := NewTokenProvider(pool, cipher, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			refreshCalls.Add(1)
			return Token{AccessToken: "access-key-retry", RefreshToken: "refresh-key-retry", ExpiresIn: 3600}, nil
		},
	})
	access, err := provider.Token(context.Background(), installationID)
	if err != nil {
		t.Fatal(err)
	}
	if access.Value != "access-key-retry" || access.TokenVersion != 2 {
		t.Fatalf("unexpected token after key-change retry: %#v", access)
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
	assertStoredRefresh(t, pool, keys, installationID, "refresh-key-retry", 2)
}

type versionFlipCipher struct {
	inner Cipher
	seals atomic.Int32
}

func (c *versionFlipCipher) Seal(plaintext, additionalData []byte) ([]byte, int, error) {
	ciphertext, version, err := c.inner.Seal(plaintext, additionalData)
	if err != nil {
		return nil, 0, err
	}
	if c.seals.Add(1) == 2 {
		return ciphertext, version + 1, nil
	}
	return ciphertext, version, nil
}

func (c *versionFlipCipher) Open(keyVersion int, ciphertext, additionalData []byte) ([]byte, error) {
	return c.inner.Open(keyVersion, ciphertext, additionalData)
}

func assertStoredRefresh(
	t *testing.T,
	pool *pgxpool.Pool,
	keys *cryptox.KeyRing,
	installationID uuid.UUID,
	wantRefresh string,
	wantVersion int64,
) {
	t.Helper()
	var refreshCiphertext []byte
	var keyVersion int
	var tokenVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT refresh_token_ciphertext, key_version, token_version
		FROM oauth_credentials WHERE installation_id=$1`, installationID,
	).Scan(&refreshCiphertext, &keyVersion, &tokenVersion); err != nil {
		t.Fatal(err)
	}
	refresh, err := keys.Open(keyVersion, refreshCiphertext, credentialsAAD(installationID))
	if err != nil {
		t.Fatal(err)
	}
	if string(refresh) != wantRefresh || tokenVersion != wantVersion {
		t.Fatalf("stored refresh=%q version=%d, want %q version=%d", refresh, tokenVersion, wantRefresh, wantVersion)
	}
}
