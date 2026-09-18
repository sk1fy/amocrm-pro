package amocrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// accountTokenProvider hands out one integration installed in several
// accounts, which is exactly the case the pair budget must separate.
type accountTokenProvider struct {
	baseURL  string
	accounts map[uuid.UUID]int64
}

func (p *accountTokenProvider) Token(_ context.Context, installationID uuid.UUID) (AccessToken, error) {
	return AccessToken{
		InstallationID: installationID,
		IntegrationID:  uuid.MustParse("ebc58cb3-a0b9-4c4b-a9b7-c2b3d8d456ba"),
		AccountID:      p.accounts[installationID],
		AccountDomain:  p.baseURL,
		Value:          "token",
		TokenVersion:   1,
	}, nil
}

func (p *accountTokenProvider) RefreshIfCurrent(_ context.Context, observed AccessToken) (AccessToken, error) {
	return observed, nil
}

func (p *accountTokenProvider) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	return nil
}

// The same integration installed in two accounts must run both accounts at
// the full pair rate instead of sharing one budget between them.
func TestOneIntegrationInTwoAccountsRunsBothAtFullRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":11,"pipeline_id":1,"status_id":2}`))
	}))
	defer server.Close()
	first, second := uuid.New(), uuid.New()
	provider := &accountTokenProvider{baseURL: server.URL, accounts: map[uuid.UUID]int64{first: 1, second: 2}}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	client.limiter = pairBoundLimiter(t, 20)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	var wg sync.WaitGroup
	for _, installation := range []uuid.UUID{first, second} {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := client.GetLeadState(ctx, installation, 11); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()

	// Two independent 20 rps budgets need about 450ms for ten calls each; one
	// shared budget would need about 950ms for the same twenty calls.
	if elapsed := time.Since(started); elapsed > 700*time.Millisecond {
		t.Fatalf("accounts shared one budget: twenty calls took %s", elapsed)
	}
}

// Every request that actually reaches amoCRM is debited, including the retry
// that follows an OAuth token refresh.
func TestRetryAfterTokenRefreshDebitsBudgetTwice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	registry := prometheus.NewRegistry()
	client.SetMetrics(NewMetrics(registry))

	if err := client.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if admitted := histogramOutcome(families, "amocrm_budget_wait_seconds", "admitted"); admitted != 2 {
		t.Fatalf("budget admissions=%d, want one per HTTP request", admitted)
	}
	assertFiniteLabels(t, families)
}

// Bootstrap discovery debits the canonical pair once the account id is known,
// so the first call of that pair is not free.
func TestBootstrapDiscoveryDebitsTheCanonicalPair(t *testing.T) {
	client, _, _ := bootstrapClientFixture(t, 200, 42)
	client.limiter = pairBoundLimiter(t, 20)
	integration := uuid.MustParse("ebc58cb3-a0b9-4c4b-a9b7-c2b3d8d456ba")

	account, err := client.BootstrapAccount(context.Background(), integration, 0, "fixture.amocrm.ru", "candidate")
	if err != nil || account.ID != 42 {
		t.Fatalf("bootstrap=%+v %v", account, err)
	}
	started := time.Now()
	if _, err := client.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 30*time.Millisecond {
		t.Fatal("the discovered pair budget still held a full token")
	}
}

// Overload is reported as a retryable classified error, never as a silent
// unbounded wait.
func TestSaturatedBudgetSurfacesOverloadToCallers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":11,"pipeline_id":1,"status_id":2}`))
	}))
	defer server.Close()
	config := DefaultLimiterConfig()
	config.PairRPS = 1
	config.MaxWait = 200 * time.Millisecond
	client, err := NewClientWithLimits(server.Client(), &fakeTokenProvider{baseURL: server.URL}, config)
	if err != nil {
		t.Fatal(err)
	}
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	registry := prometheus.NewRegistry()
	client.SetMetrics(NewMetrics(registry))

	ctx := context.Background()
	if _, err := client.GetLeadState(ctx, uuid.New(), 11); err != nil {
		t.Fatal(err)
	}
	_, err = client.GetLeadState(ctx, uuid.New(), 11)
	var api *APIError
	if !errors.As(err, &api) || api.Kind != ErrorOverloaded || !api.Retryable {
		t.Fatalf("saturated budget returned %v, want a retryable overload", err)
	}
	families, gatherErr := registry.Gather()
	if gatherErr != nil {
		t.Fatal(gatherErr)
	}
	if shed := histogramOutcome(families, "amocrm_budget_wait_seconds", "overloaded"); shed != 1 {
		t.Fatalf("overload observations=%d, want 1", shed)
	}
	assertFiniteLabels(t, families)
}
