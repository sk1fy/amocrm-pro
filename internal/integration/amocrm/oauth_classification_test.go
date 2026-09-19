package amocrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestOAuthRejectsOnlyExplicitInvalidGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
	}{
		{"invalid grant", 400, `{"error":"invalid_grant","description":"discarded"}`, ErrorInvalidGrant},
		{"validation", 400, `{"error":"invalid_request"}`, ErrorValidation},
		{"unprocessable", 422, `{"error":"invalid_request"}`, ErrorValidation},
		{"unauthorized", 401, `{}`, ErrorUnauthorized},
		{"temporary", 503, `{}`, ErrorTemporary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := NewOAuthClient(server.Client())
			client.resolveAccount = func(string) (*url.URL, error) { return url.Parse(server.URL) }
			_, err := client.Refresh(context.Background(), "fixture.amocrm.test", OAuthCredentials{ClientID: "fixture", ClientSecret: "fixture", RedirectURI: "https://fixture.example.invalid"}, "fixture")
			var api *APIError
			if !errors.As(err, &api) || api.Kind != tc.kind {
				t.Fatalf("error=%v want %s", err, tc.kind)
			}
		})
	}
}
