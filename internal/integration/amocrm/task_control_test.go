package amocrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

func TestTaskControlProbeKeepsCurrentTokenDenial(t *testing.T) {
	for _, code := range []int{200, 401, 403, 429, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v4/account" {
					t.Error("unexpected probe")
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"id":42}`))
			}))
			defer server.Close()
			provider := &fakeTokenProvider{baseURL: server.URL}
			client := NewClient(server.Client(), provider)
			client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
			err := client.CheckTaskControlAuthorization(context.Background(), uuid.New(), 42)
			if code == 200 && err != nil {
				t.Fatal(err)
			}
			if code != 200 {
				var api *APIError
				if !errors.As(err, &api) || api.StatusCode != code {
					t.Fatal(err)
				}
			}
			if len(provider.requests) != 1 || provider.markedReauth != 0 {
				t.Fatal("probe rotated or revoked credentials")
			}
		})
	}
}
