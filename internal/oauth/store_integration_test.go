package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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

const oauthTestEncryptionKeys = "1:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func TestStoreConsumesOAuthStateOnceConcurrently(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	state, _, err := store.CreateState(context.Background(), integration.ID, "/widget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByCaller := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, consumeErr := store.ConsumeState(context.Background(), state)
			errorsByCaller <- consumeErr
		}()
	}
	close(start)

	successes := 0
	replays := 0
	for range 2 {
		consumeErr := <-errorsByCaller
		switch {
		case consumeErr == nil:
			successes++
		case errors.Is(consumeErr, ErrInvalidState):
			replays++
		default:
			t.Fatalf("unexpected state consumption error: %v", consumeErr)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("state results: successes=%d replays=%d", successes, replays)
	}
}

func TestStoreSavesInstallationCredentialsJobAndAuditAtomically(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)

	result, err := store.SaveInstallation(context.Background(), integration, Account{
		ID: 42, Subdomain: "tenant",
	}, "tenant.amocrm.ru", Token{
		AccessToken: "access-one", RefreshToken: "refresh-one", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccountID != 42 || result.Status != "active" {
		t.Fatalf("unexpected installation result: %#v", result)
	}

	var accessCiphertext, refreshCiphertext []byte
	var keyVersion int
	var tokenVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT access_token_ciphertext, refresh_token_ciphertext, key_version, token_version
		FROM oauth_credentials WHERE installation_id=$1`, result.ID,
	).Scan(&accessCiphertext, &refreshCiphertext, &keyVersion, &tokenVersion); err != nil {
		t.Fatal(err)
	}
	access, err := keys.Open(keyVersion, accessCiphertext, credentialsAAD(result.ID))
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := keys.Open(keyVersion, refreshCiphertext, credentialsAAD(result.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(access) != "access-one" || string(refresh) != "refresh-one" || tokenVersion != 1 {
		t.Fatalf("unexpected stored credentials: access=%q refresh=%q version=%d", access, refresh, tokenVersion)
	}

	var reconcileJobs, auditRecords int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM jobs WHERE installation_id=$1 AND type='webhook.reconcile'),
			(SELECT count(*) FROM audit_log WHERE installation_id=$1 AND action='installation.authorized')`,
		result.ID,
	).Scan(&reconcileJobs, &auditRecords); err != nil {
		t.Fatal(err)
	}
	if reconcileJobs != 1 || auditRecords != 1 {
		t.Fatalf("atomic side effects: jobs=%d audit=%d", reconcileJobs, auditRecords)
	}
}

func TestStoreRollsBackAuthorizationWhenReconcileEnqueueFails(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION oauth_test_fail_reconcile_job() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.type = 'webhook.reconcile' THEN
				RAISE EXCEPTION 'synthetic reconcile enqueue failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER oauth_test_fail_reconcile_job
		BEFORE INSERT ON jobs
		FOR EACH ROW EXECUTE FUNCTION oauth_test_fail_reconcile_job()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupContext, `
			DROP TRIGGER IF EXISTS oauth_test_fail_reconcile_job ON jobs;
			DROP FUNCTION IF EXISTS oauth_test_fail_reconcile_job()`)
	})

	_, err := store.SaveInstallation(ctx, integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{
		AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600,
	})
	if err == nil {
		t.Fatal("expected synthetic reconcile enqueue failure")
	}
	var installations, credentials, jobsCount, audits int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM installations WHERE integration_id=$1),
			(SELECT count(*) FROM oauth_credentials),
			(SELECT count(*) FROM jobs),
			(SELECT count(*) FROM audit_log)`, integration.ID,
	).Scan(&installations, &credentials, &jobsCount, &audits); err != nil {
		t.Fatal(err)
	}
	if installations != 0 || credentials != 0 || jobsCount != 0 || audits != 0 {
		t.Fatalf("authorization was not rolled back: installations=%d credentials=%d jobs=%d audit=%d", installations, credentials, jobsCount, audits)
	}
}

func TestTokenProviderCoordinatesConcurrentRefresh(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)

	var refreshCalls atomic.Int32
	var receivedRefreshToken atomic.Value
	gateway := &oauthTestGateway{
		refresh: func(_ context.Context, _, _, _, _, refreshToken string) (Token, error) {
			refreshCalls.Add(1)
			receivedRefreshToken.Store(refreshToken)
			time.Sleep(75 * time.Millisecond)
			return Token{AccessToken: "access-rotated", RefreshToken: "refresh-rotated", ExpiresIn: 3600}, nil
		},
	}
	provider := NewTokenProvider(pool, keys, gateway)

	const callers = 8
	start := make(chan struct{})
	results := make(chan amocrm.AccessToken, callers)
	errorsByCaller := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			<-start
			token, err := provider.Token(context.Background(), installationID)
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
		if token.Value != "access-rotated" || token.InstallationID != installationID {
			t.Fatalf("unexpected refreshed token: %#v", token)
		}
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
	if received, _ := receivedRefreshToken.Load().(string); received != "refresh-initial" {
		t.Fatalf("remote refresh token = %q, want initial token", received)
	}
	var tokenVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT token_version FROM oauth_credentials WHERE installation_id=$1`, installationID,
	).Scan(&tokenVersion); err != nil {
		t.Fatal(err)
	}
	if tokenVersion != 2 {
		t.Fatalf("token version = %d, want 2", tokenVersion)
	}
}

func TestClientStaggered401RefreshesObservedVersionOnce(t *testing.T) {
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
	secondOldStarted := make(chan struct{})
	firstNewUsed := make(chan struct{})
	var closeSecond sync.Once
	var closeFirstNew sync.Once
	transport := oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		caller, _ := request.Context().Value(oauthCallerKey{}).(string)
		authorization := request.Header.Get("Authorization")
		switch authorization {
		case "Bearer access-initial":
			if caller == "second" {
				closeSecond.Do(func() { close(secondOldStarted) })
				<-firstNewUsed
			} else {
				<-secondOldStarted
			}
			return oauthHTTPResponse(http.StatusUnauthorized, ""), nil
		case "Bearer access-rotated":
			if caller == "first" {
				closeFirstNew.Do(func() { close(firstNewUsed) })
			}
			return oauthHTTPResponse(http.StatusOK, `{"ok":true}`), nil
		default:
			return nil, errors.New("unexpected authorization token")
		}
	})
	client := amocrm.NewClient(&http.Client{Transport: transport, Timeout: 2 * time.Second}, provider)

	start := make(chan struct{})
	errorsByCaller := make(chan error, 2)
	for _, caller := range []string{"first", "second"} {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), oauthCallerKey{}, caller), 5*time.Second)
			defer cancel()
			var response map[string]bool
			errorsByCaller <- client.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/test", nil, &response)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsByCaller; err != nil {
			t.Fatalf("staggered 401 request: %v", err)
		}
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("remote refresh calls = %d, want 1", calls)
	}
	var tokenVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT token_version FROM oauth_credentials WHERE installation_id=$1`, installationID,
	).Scan(&tokenVersion); err != nil {
		t.Fatal(err)
	}
	if tokenVersion != 2 {
		t.Fatalf("token version = %d, want 2", tokenVersion)
	}
}

func TestTokenProviderPersistsRotationAfterCallerCancellation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)
	ctx, cancel := context.WithCancel(context.Background())
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			cancel()
			return Token{AccessToken: "access-after-cancel", RefreshToken: "refresh-after-cancel", ExpiresIn: 3600}, nil
		},
	})

	access, err := provider.Token(ctx, installationID)
	if err != nil {
		t.Fatal(err)
	}
	if access.Value != "access-after-cancel" || access.TokenVersion != 2 {
		t.Fatalf("unexpected finalized token: %#v", access)
	}
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
	if string(refresh) != "refresh-after-cancel" || tokenVersion != 2 {
		t.Fatalf("rotation was not finalized: refresh=%q version=%d", refresh, tokenVersion)
	}
}

func TestMarkReauthRequiredIsVersionFenced(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	first, err := store.SaveInstallation(context.Background(), integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{
		AccessToken: "access-v1", RefreshToken: "refresh-v1", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveInstallation(context.Background(), integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{
		AccessToken: "access-v2", RefreshToken: "refresh-v2", ExpiresIn: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("reauthorization changed installation id: %s -> %s", first.ID, second.ID)
	}
	provider := NewTokenProvider(pool, keys, nil)
	if err := provider.MarkReauthRequired(context.Background(), first.ID, 1); err != nil {
		t.Fatal(err)
	}
	assertOAuthInstallationStatus(t, pool, first.ID, "active")
	if err := provider.MarkReauthRequired(context.Background(), first.ID, 2); err != nil {
		t.Fatal(err)
	}
	assertOAuthInstallationStatus(t, pool, first.ID, "reauth_required")

	if _, err := pool.Exec(context.Background(), `UPDATE installations SET status='uninstalled' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := provider.MarkReauthRequired(context.Background(), first.ID, 2); err != nil {
		t.Fatal(err)
	}
	assertOAuthInstallationStatus(t, pool, first.ID, "uninstalled")
}

func TestUnauthorizedRefreshDoesNotDeadlockReauthorization(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	initial, err := store.SaveInstallation(context.Background(), integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{
		AccessToken: "access-v1", RefreshToken: "refresh-v1", ExpiresIn: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseRefreshOnce sync.Once
	t.Cleanup(func() { releaseRefreshOnce.Do(func() { close(releaseRefresh) }) })
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			close(refreshEntered)
			<-releaseRefresh
			return Token{}, &amocrm.APIError{Kind: amocrm.ErrorUnauthorized, StatusCode: http.StatusUnauthorized}
		},
	})
	refreshError := make(chan error, 1)
	refreshContext, cancelRefresh := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRefresh()
	go func() {
		_, refreshErr := provider.Token(refreshContext, initial.ID)
		refreshError <- refreshErr
	}()
	<-refreshEntered

	reauthorizationError := make(chan error, 1)
	reauthorizationContext, cancelReauthorization := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReauthorization()
	go func() {
		_, saveErr := store.SaveInstallation(reauthorizationContext, integration, Account{ID: 42, Subdomain: "tenant"}, "tenant.amocrm.ru", Token{
			AccessToken: "access-v2", RefreshToken: "refresh-v2", ExpiresIn: 3600,
		})
		reauthorizationError <- saveErr
	}()
	waitForDatabaseLock(t, pool, "INSERT INTO oauth_credentials")
	releaseRefreshOnce.Do(func() { close(releaseRefresh) })

	if err := <-refreshError; err == nil {
		t.Fatal("expected unauthorized refresh error")
	}
	if err := <-reauthorizationError; err != nil {
		t.Fatalf("reauthorization failed: %v", err)
	}
	assertOAuthInstallationStatus(t, pool, initial.ID, "active")
	var tokenVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT token_version FROM oauth_credentials WHERE installation_id=$1`, initial.ID,
	).Scan(&tokenVersion); err != nil {
		t.Fatal(err)
	}
	if tokenVersion != 2 {
		t.Fatalf("token version = %d, want 2", tokenVersion)
	}
}

func TestSuccessfulRefreshWinsConcurrentReauthMark(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys := oauthTestKeyRing(t)
	installationID := oauthTestInstallation(t, pool, keys, -1)
	refreshEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseRefreshOnce sync.Once
	t.Cleanup(func() { releaseRefreshOnce.Do(func() { close(releaseRefresh) }) })
	provider := NewTokenProvider(pool, keys, &oauthTestGateway{
		refresh: func(context.Context, string, string, string, string, string) (Token, error) {
			close(refreshEntered)
			<-releaseRefresh
			return Token{AccessToken: "access-v2", RefreshToken: "refresh-v2", ExpiresIn: 3600}, nil
		},
	})
	testContext, cancelTest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTest()
	refreshError := make(chan error, 1)
	go func() {
		_, refreshErr := provider.Token(testContext, installationID)
		refreshError <- refreshErr
	}()
	<-refreshEntered
	markError := make(chan error, 1)
	go func() {
		markError <- provider.MarkReauthRequired(testContext, installationID, 1)
	}()
	waitForDatabaseLock(t, pool, "SELECT token_version FROM oauth_credentials")
	releaseRefreshOnce.Do(func() { close(releaseRefresh) })

	if err := <-refreshError; err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if err := <-markError; err != nil {
		t.Fatalf("version-fenced mark failed: %v", err)
	}
	assertOAuthInstallationStatus(t, pool, installationID, "active")
}

func TestTokenProviderRefreshFailureState(t *testing.T) {
	tests := []struct {
		name       string
		refreshErr error
		wantStatus string
	}{
		{
			name: "unauthorized requires reauthorization",
			refreshErr: &amocrm.APIError{
				Kind: amocrm.ErrorUnauthorized, StatusCode: 401,
			},
			wantStatus: "reauth_required",
		},
		{
			name: "transient failure preserves active installation",
			refreshErr: &amocrm.APIError{
				Kind: amocrm.ErrorTemporary, StatusCode: 503, Retryable: true,
			},
			wantStatus: "active",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			keys := oauthTestKeyRing(t)
			installationID := oauthTestInstallation(t, pool, keys, -1)
			provider := NewTokenProvider(pool, keys, &oauthTestGateway{
				refresh: func(context.Context, string, string, string, string, string) (Token, error) {
					return Token{}, test.refreshErr
				},
			})

			if _, err := provider.Token(context.Background(), installationID); err == nil {
				t.Fatal("expected refresh error")
			}
			var installationStatus string
			var tokenVersion int64
			if err := pool.QueryRow(context.Background(), `
				SELECT installation.status, credentials.token_version
				FROM installations installation
				JOIN oauth_credentials credentials ON credentials.installation_id=installation.id
				WHERE installation.id=$1`, installationID,
			).Scan(&installationStatus, &tokenVersion); err != nil {
				t.Fatal(err)
			}
			if installationStatus != test.wantStatus || tokenVersion != 1 {
				t.Fatalf("failure state: status=%s token_version=%d", installationStatus, tokenVersion)
			}
		})
	}
}

type oauthTestGateway struct {
	refresh func(context.Context, string, string, string, string, string) (Token, error)
}

type oauthCallerKey struct{}

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip oauthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func oauthHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (g *oauthTestGateway) ExchangeCode(context.Context, string, string, string, string, string) (Token, error) {
	return Token{}, errors.New("unexpected code exchange")
}

func (g *oauthTestGateway) Refresh(
	ctx context.Context,
	accountDomain, clientID, clientSecret, redirectURI, refreshToken string,
) (Token, error) {
	return g.refresh(ctx, accountDomain, clientID, clientSecret, redirectURI, refreshToken)
}

func (g *oauthTestGateway) GetAccount(context.Context, string, string) (Account, error) {
	return Account{}, errors.New("unexpected account lookup")
}

func oauthTestKeyRing(t *testing.T) *cryptox.KeyRing {
	t.Helper()
	keys, err := cryptox.ParseKeyRing(oauthTestEncryptionKeys, 1)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func oauthTestIntegration(t *testing.T, store *Store) Integration {
	t.Helper()
	integration, err := store.EnsureIntegration(context.Background(), IntegrationInput{
		Code:          "oauth-test",
		ClientID:      uuid.NewString(),
		ClientSecret:  "synthetic-client-secret",
		RedirectURI:   "https://service.example.test/oauth/amocrm/callback",
		WebhookEvents: []string{"update_lead", "add_contact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return integration
}

func oauthTestInstallation(
	t *testing.T,
	pool *pgxpool.Pool,
	keys *cryptox.KeyRing,
	expiresIn int64,
) uuid.UUID {
	t.Helper()
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	result, err := store.SaveInstallation(context.Background(), integration, Account{
		ID: 42, Subdomain: "tenant",
	}, "tenant.amocrm.ru", Token{
		AccessToken: "access-initial", RefreshToken: "refresh-initial", ExpiresIn: expiresIn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.ID
}

func assertOAuthInstallationStatus(
	t *testing.T,
	pool *pgxpool.Pool,
	installationID uuid.UUID,
	want string,
) {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `
		SELECT status FROM installations WHERE id=$1`, installationID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("installation status = %q, want %q", status, want)
	}
}

func waitForDatabaseLock(t *testing.T, pool *pgxpool.Pool, queryFragment string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database()
			  AND wait_event_type='Lock'
			  AND query LIKE '%' || $1 || '%'`, queryFragment,
		).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query containing %q did not reach a database lock wait", queryFragment)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
