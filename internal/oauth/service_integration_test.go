package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestServiceCallbackConsumesStateOnceConcurrently(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	state, _, err := store.CreateState(context.Background(), integration.ID, "/widget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var exchangeCalls atomic.Int32
	gateway := &serviceCallbackGateway{
		exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
			exchangeCalls.Add(1)
			return Token{
				AccessToken:  "callback-access",
				RefreshToken: "callback-refresh",
				ExpiresIn:    3600,
			}, nil
		},
		getAccount: func(context.Context, string, string) (Account, error) {
			return Account{ID: 501, Subdomain: "callback-race"}, nil
		},
	}
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)

	start := make(chan struct{})
	results := make(chan InstallationResult, 2)
	errorsByCaller := make(chan error, 2)
	var callers sync.WaitGroup
	callers.Add(2)
	for range 2 {
		go func() {
			defer callers.Done()
			<-start
			result, callbackErr := service.Callback(
				context.Background(),
				state,
				"single-use-code",
				"https://callback-race.amocrm.ru",
			)
			results <- result
			errorsByCaller <- callbackErr
		}()
	}
	close(start)
	callers.Wait()
	close(results)
	close(errorsByCaller)

	successes := 0
	replays := 0
	for callbackErr := range errorsByCaller {
		switch {
		case callbackErr == nil:
			successes++
		case errors.Is(callbackErr, ErrInvalidState):
			replays++
		default:
			t.Fatalf("unexpected callback error: %v", callbackErr)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("callback results: successes=%d replays=%d", successes, replays)
	}
	if calls := exchangeCalls.Load(); calls != 1 {
		t.Fatalf("code exchange calls = %d, want 1", calls)
	}

	nonEmptyResults := 0
	for result := range results {
		if result.ID != uuid.Nil {
			nonEmptyResults++
			if result.AccountID != 501 || result.Status != "active" {
				t.Fatalf("unexpected successful callback result: %#v", result)
			}
		}
	}
	if nonEmptyResults != 1 {
		t.Fatalf("non-empty callback results = %d, want 1", nonEmptyResults)
	}
}

func TestServiceCallbackAtomicallyPersistsAuthorization(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	state, _, err := store.CreateState(context.Background(), integration.ID, "/widget/authorized", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var exchangeAccountDomain, exchangeClientID, exchangeClientSecret string
	var exchangeRedirectURI, exchangeCode, accountLookupToken string
	gateway := &serviceCallbackGateway{
		exchangeCode: func(
			_ context.Context,
			accountDomain, clientID, clientSecret, redirectURI, code string,
		) (Token, error) {
			exchangeAccountDomain = accountDomain
			exchangeClientID = clientID
			exchangeClientSecret = clientSecret
			exchangeRedirectURI = redirectURI
			exchangeCode = code
			return Token{
				AccessToken:  "service-access-token",
				RefreshToken: "service-refresh-token",
				ExpiresIn:    7200,
			}, nil
		},
		getAccount: func(_ context.Context, accountDomain, accessToken string) (Account, error) {
			if accountDomain != "callback-success.amocrm.ru" {
				t.Fatalf("account lookup domain = %q", accountDomain)
			}
			accountLookupToken = accessToken
			return Account{ID: 90210, Subdomain: "callback-success"}, nil
		},
	}
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)

	result, err := service.Callback(
		context.Background(),
		state,
		"authorization-code",
		"https://callback-success.amocrm.ru/",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == uuid.Nil || result.AccountID != 90210 || result.Status != "active" {
		t.Fatalf("unexpected installation result: %#v", result)
	}
	if exchangeAccountDomain != "callback-success.amocrm.ru" ||
		exchangeClientID != integration.ClientID ||
		exchangeClientSecret != "synthetic-client-secret" ||
		exchangeRedirectURI != integration.RedirectURI ||
		exchangeCode != "authorization-code" ||
		accountLookupToken != "service-access-token" {
		t.Fatalf(
			"unexpected gateway arguments: domain=%q client_id=%q secret=%q redirect=%q code=%q account_token=%q",
			exchangeAccountDomain,
			exchangeClientID,
			exchangeClientSecret,
			exchangeRedirectURI,
			exchangeCode,
			accountLookupToken,
		)
	}

	var accountID int64
	var accountDomain, installationStatus, webhookStatus string
	var webhookSettings []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT account_id, account_domain, status, webhook_status, webhook_settings
		FROM installations WHERE id=$1`, result.ID,
	).Scan(&accountID, &accountDomain, &installationStatus, &webhookStatus, &webhookSettings); err != nil {
		t.Fatal(err)
	}
	var events []string
	if err := json.Unmarshal(webhookSettings, &events); err != nil {
		t.Fatal(err)
	}
	if accountID != 90210 || accountDomain != "callback-success.amocrm.ru" ||
		installationStatus != "active" || webhookStatus != "pending" ||
		len(events) != 2 || events[0] != "update_lead" || events[1] != "add_contact" {
		t.Fatalf(
			"unexpected installation: account_id=%d domain=%q status=%q webhook_status=%q events=%v",
			accountID,
			accountDomain,
			installationStatus,
			webhookStatus,
			events,
		)
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
	accessToken, err := keys.Open(keyVersion, accessCiphertext, credentialsAAD(result.ID))
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := keys.Open(keyVersion, refreshCiphertext, credentialsAAD(result.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(accessToken) != "service-access-token" ||
		string(refreshToken) != "service-refresh-token" || tokenVersion != 1 {
		t.Fatalf(
			"unexpected credentials: access=%q refresh=%q version=%d",
			accessToken,
			refreshToken,
			tokenVersion,
		)
	}

	var reconcileJobs, auditRecords, stateConsumptions int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM jobs
			 WHERE installation_id=$1 AND type='webhook.reconcile'
			   AND payload->>'installation_id'=$1::text),
			(SELECT count(*) FROM audit_log
			 WHERE installation_id=$1 AND actor_type='oauth'
			   AND action='installation.authorized' AND object_id=$1::text),
			(SELECT count(*) FROM oauth_states
			 WHERE integration_id=$2 AND consumed_at IS NOT NULL)`,
		result.ID, integration.ID,
	).Scan(&reconcileJobs, &auditRecords, &stateConsumptions); err != nil {
		t.Fatal(err)
	}
	if reconcileJobs != 1 || auditRecords != 1 || stateConsumptions != 1 {
		t.Fatalf(
			"authorization side effects: reconcile_jobs=%d audit=%d consumed_states=%d",
			reconcileJobs,
			auditRecords,
			stateConsumptions,
		)
	}
}

type serviceCallbackGateway struct {
	exchangeCode func(context.Context, string, string, string, string, string) (Token, error)
	getAccount   func(context.Context, string, string) (Account, error)
}

func testkitPostgresForServiceCallback(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	return pool
}

func (g *serviceCallbackGateway) ExchangeCode(
	ctx context.Context,
	accountDomain, clientID, clientSecret, redirectURI, code string,
) (Token, error) {
	return g.exchangeCode(ctx, accountDomain, clientID, clientSecret, redirectURI, code)
}

func (*serviceCallbackGateway) Refresh(
	context.Context,
	string,
	string,
	string,
	string,
	string,
) (Token, error) {
	return Token{}, errors.New("unexpected token refresh")
}

func (g *serviceCallbackGateway) GetAccount(
	ctx context.Context,
	accountDomain, accessToken string,
) (Account, error) {
	return g.getAccount(ctx, accountDomain, accessToken)
}
