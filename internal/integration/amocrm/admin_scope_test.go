package amocrm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestScopedUninstallProviderSharesGatewayBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	base := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	base.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	base.limiter = pairBoundLimiter(t, 0.1)
	scoped := base.WithTokenProvider(&fakeTokenProvider{baseURL: server.URL})
	id := uuid.New()
	if err := base.DoJSON(t.Context(), id, http.MethodGet, "/api/v4/account", nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := scoped.DoJSON(ctx, id, http.MethodGet, "/api/v4/webhooks", nil, nil); err == nil {
		t.Fatal("scoped client minted another outbound budget")
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls=%d", calls.Load())
	}
}
