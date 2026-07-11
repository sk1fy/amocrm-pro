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
)

type fakeTokenProvider struct {
	mu            sync.Mutex
	baseURL       string
	requests      []bool
	markedReauth  int
	markedVersion int64
	mark          func(context.Context, uuid.UUID, int64) error
}

func (p *fakeTokenProvider) Token(_ context.Context, installationID uuid.UUID) (AccessToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, false)
	return AccessToken{
		InstallationID: installationID,
		IntegrationID:  uuid.MustParse("ebc58cb3-a0b9-4c4b-a9b7-c2b3d8d456ba"),
		AccountID:      42,
		AccountDomain:  p.baseURL,
		Value:          "old",
		TokenVersion:   1,
	}, nil
}

func (p *fakeTokenProvider) RefreshIfCurrent(_ context.Context, observed AccessToken) (AccessToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, true)
	observed.Value = "new"
	observed.TokenVersion = 2
	return observed, nil
}

func (p *fakeTokenProvider) MarkReauthRequired(ctx context.Context, installationID uuid.UUID, version int64) error {
	p.mu.Lock()
	p.markedReauth++
	p.markedVersion = version
	mark := p.mark
	p.mu.Unlock()
	if mark != nil {
		return mark(ctx, installationID, version)
	}
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
	if provider.markedVersion != 2 {
		t.Fatalf("marked token version = %d, want 2", provider.markedVersion)
	}
}

func TestClientBoundsReauthorizationStatusWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	provider := &fakeTokenProvider{
		baseURL: server.URL,
		mark: func(ctx context.Context, _ uuid.UUID, _ int64) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	client.reauthTimeout = 20 * time.Millisecond

	started := time.Now()
	err := client.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/test", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded mark timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("reauthorization status write took too long: %s", elapsed)
	}
}
