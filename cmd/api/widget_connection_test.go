package main

import (
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
)

func TestWidgetConnectionBeforeInstallation(t *testing.T) {
	router := chi.NewRouter()
	registerWidgetConnection(router, nil, nil, nil)
	for _, tt := range []struct {
		name, method, origin, preflight string
		status                          int
		allowOrigin                     string
	}{
		{"amoCRM preflight", "OPTIONS", "https://example.amocrm.ru", "GET", 204, "https://example.amocrm.ru"},
		{"external origin", "OPTIONS", "https://amocrm.ru.example.org", "GET", 403, ""},
		{"unrelated preflight method", "OPTIONS", "https://example.amocrm.ru", "POST", 403, ""},
		{"credentials required", "GET", "https://example.amocrm.ru", "", 401, "https://example.amocrm.ru"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, apicontract.WidgetConnection.Path, nil)
			req.Header.Set("Origin", tt.origin)
			if tt.preflight != "" {
				req.Header.Set("Access-Control-Request-Method", tt.preflight)
				req.Header.Set("Access-Control-Request-Headers", "x-auth-token")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tt.status || response.Header().Get("Access-Control-Allow-Origin") != tt.allowOrigin {
				t.Fatalf("status %d, allow-origin %q", response.Code, response.Header().Get("Access-Control-Allow-Origin"))
			}
		})
	}
}
