package amocrm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type fakeTokenProvider struct {
	mu           sync.Mutex
	baseURL      string
	requests     []bool
	markedReauth int
}

func (p *fakeTokenProvider) Token(_ context.Context, installationID uuid.UUID, force bool) (AccessToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, force)
	value := "old"
	if force {
		value = "new"
	}
	return AccessToken{
		InstallationID: installationID,
		IntegrationID:  uuid.MustParse("ebc58cb3-a0b9-4c4b-a9b7-c2b3d8d456ba"),
		AccountID:      42,
		AccountDomain:  p.baseURL,
		Value:          value,
	}, nil
}

func (p *fakeTokenProvider) MarkReauthRequired(context.Context, uuid.UUID) error {
	p.mu.Lock()
	p.markedReauth++
	p.mu.Unlock()
	return nil
}

func TestClientRefreshesAndRetries401Once(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	provider := &fakeTokenProvider{baseURL: server.URL}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	var response map[string]bool
	err := client.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/test", nil, &response)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected exactly 2 HTTP requests, got %d", requests)
	}
	if len(provider.requests) != 2 || provider.requests[0] || !provider.requests[1] {
		t.Fatalf("unexpected token calls: %#v", provider.requests)
	}
	if !response["ok"] {
		t.Fatal("response was not decoded")
	}
}

func TestClientMarksReauthAfterSecond401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	provider := &fakeTokenProvider{baseURL: server.URL}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }

	err := client.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/test", nil, nil)
	apiError, ok := err.(*APIError)
	if !ok || apiError.Kind != ErrorUnauthorized {
		t.Fatalf("unexpected error: %#v", err)
	}
	if provider.markedReauth != 1 {
		t.Fatalf("expected installation to be marked once, got %d", provider.markedReauth)
	}
}
