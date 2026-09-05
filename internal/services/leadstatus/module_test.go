package leadstatus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
)

type moduleOriginAuthorizer struct{}

func (moduleOriginAuthorizer) IsActiveOrigin(_ context.Context, origin string) (bool, error) {
	return origin == "https://module.amocrm.ru", nil
}

func TestModuleRegistersOwnPreflightsBeforeAuthentication(t *testing.T) {
	module := NewModule(nil, nil)
	routes := module.Routes()
	if Code != "lead-status" || len(routes) != 2 ||
		routes[0].Path != "/api/v1/widget/actions/leads/set-status" ||
		routes[1].Path != "/api/v1/widget/workflow-rules/lead-status/configure" {
		t.Fatalf("module contract: code=%s routes=%v", Code, routes)
	}
	routes[0].Path = "/mutated"
	if module.Routes()[0].Path == "/mutated" {
		t.Fatal("route metadata leaked mutable module state")
	}
	router := chi.NewRouter()
	authenticationCalls := 0
	cors := widgetcors.Middleware(moduleOriginAuthorizer{})
	module.RegisterHTTP(router, func(next http.Handler) http.Handler {
		return cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authenticationCalls++
			if r.Header.Get("X-Auth-Token") == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		}))
	})
	for _, route := range module.Routes() {
		request := httptest.NewRequest(http.MethodOptions, route.Path, nil)
		request.Header.Set("Origin", "https://module.amocrm.ru")
		request.Header.Set("Access-Control-Request-Method", route.Method)
		request.Header.Set("Access-Control-Request-Headers", "X-Auth-Token, Idempotency-Key")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "https://module.amocrm.ru" || authenticationCalls != 0 {
			t.Fatalf("preflight %s: code=%d headers=%v authentication=%d", route.Path, response.Code, response.Header(), authenticationCalls)
		}
	}
	for _, route := range module.Routes() {
		request := httptest.NewRequest(route.Method, route.Path, nil)
		request.Header.Set("Origin", "https://module.amocrm.ru")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated command %s: %d", route.Path, response.Code)
		}
	}
	if authenticationCalls != 2 {
		t.Fatalf("commands bypassed authentication: calls=%d", authenticationCalls)
	}
}
