package amocrm

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func bootstrapClientFixture(t *testing.T, status int, accountID int64) (*Client, *fakeTokenProvider, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v4/account" {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(status)
			if status == 200 {
				_, _ = w.Write([]byte(`{"id":` + fmtInt(accountID) + `,"subdomain":"fixture"}`))
			}
			return
		}
		if r.URL.Path != "/api/v4/events" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"events":[]}}`))
	}))
	t.Cleanup(server.Close)
	provider := &fakeTokenProvider{baseURL: server.URL}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(string) (*url.URL, error) { return url.Parse(server.URL) }
	return client, provider, calls
}
func fmtInt(value int64) string { return fmt.Sprintf("%d", value) }

func TestBootstrapKnownAccountSharesExistingIntegrationAndAccountBudgets(t *testing.T) {
	for _, kind := range []string{"same integration", "same account across integrations"} {
		t.Run(kind, func(t *testing.T) {
			client, _, _ := bootstrapClientFixture(t, 200, 42)
			integration := uuid.MustParse("ebc58cb3-a0b9-4c4b-a9b7-c2b3d8d456ba")
			if kind == "same integration" {
				client.limiter = newLimiter(20, 1, rate.Inf, 1000)
			} else {
				client.limiter = newLimiter(rate.Inf, 1000, 20, 1)
				integration = uuid.New()
			}
			if _, err := client.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			account, err := client.BootstrapAccount(context.Background(), integration, 42, "fixture.amocrm.ru", "transient-candidate")
			if err != nil || account.ID != 42 {
				t.Fatalf("account=%+v err=%v", account, err)
			}
			if time.Since(started) < 30*time.Millisecond {
				t.Fatalf("bootstrap bypassed shared %s budget", kind)
			}
		})
	}
}

func TestBootstrapDiscoveryDebitsCanonicalAccountBeforeNormalTraffic(t *testing.T) {
	client, provider, _ := bootstrapClientFixture(t, 200, 42)
	client.limiter = newLimiter(rate.Inf, 1000, 20, 1)
	account, err := client.BootstrapAccount(context.Background(), uuid.New(), 0, "fixture.amocrm.ru", "candidate")
	if err != nil || account.ID != 42 {
		t.Fatalf("bootstrap=%+v %v", account, err)
	}
	if len(provider.requests) != 0 {
		t.Fatal("bootstrap used stored token provider")
	}
	if client.bootstrapAccounts["fixture.amocrm.ru"] != 42 {
		t.Fatal("did not bind discovered account identity")
	}
	started := time.Now()
	if _, err := client.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 30*time.Millisecond {
		t.Fatal("discovered account got a fresh additional burst")
	}
	started = time.Now()
	if _, err := client.BootstrapAccount(context.Background(), uuid.New(), 0, "fixture.amocrm.ru", "another-candidate"); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 30*time.Millisecond {
		t.Fatal("repeat discovery bypassed canonical account bucket")
	}
}
func TestBootstrapCancellationRateLimitAndIdentityMismatch(t *testing.T) {
	client, _, calls := bootstrapClientFixture(t, 429, 42)
	_, err := client.BootstrapAccount(context.Background(), uuid.New(), 42, "fixture.amocrm.ru", "candidate")
	var api *APIError
	if !errors.As(err, &api) || api.Kind != ErrorRateLimited || api.RetryAfter != 9*time.Second {
		t.Fatalf("429=%v", err)
	}
	before := calls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.BootstrapAccount(ctx, uuid.New(), 42, "fixture.amocrm.ru", "candidate"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if calls.Load() != before {
		t.Fatal("canceled bootstrap reached upstream")
	}
	client, _, _ = bootstrapClientFixture(t, 200, 43)
	if _, err := client.BootstrapAccount(context.Background(), uuid.New(), 42, "fixture.amocrm.ru", "candidate"); !errors.Is(err, ErrIncompleteResponse) {
		t.Fatalf("accepted known-account mismatch: %v", err)
	}
}
