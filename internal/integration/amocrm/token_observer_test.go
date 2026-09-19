package amocrm

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func TestCredentialObserverUsesSharedProviderAndRefreshedVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer server.Close()
	provider := &fakeTokenProvider{baseURL: server.URL}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(string) (*url.URL, error) { return url.Parse(server.URL) }
	versions := []int64{}
	observed := client.WithCredentialVersionObserver(func(v int64) { versions = append(versions, v) })
	if err := observed.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/account", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(versions, []int64{1, 2}) || !slices.Equal(provider.requests, []bool{false, true}) {
		t.Fatalf("versions=%v provider=%v", versions, provider.requests)
	}
	if observed.limiter != client.limiter {
		t.Fatal("observation created an independent outgoing budget")
	}
}
