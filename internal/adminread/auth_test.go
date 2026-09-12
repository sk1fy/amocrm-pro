package adminread

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

func TestAuthenticate(t *testing.T) {
	h := &handler{token: "admin-dev-credential"}
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(h.authenticate)
	router.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		if actorFrom(r.Context()) != "employee:11111111-1111-1111-1111-111111111111" {
			t.Errorf("actor=%q", actorFrom(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	get := func(auth, actor string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/ok", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if actor != "" {
			req.Header.Set("X-Admin-Actor", actor)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("", "employee:11111111-1111-1111-1111-111111111111"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token=%d %s", rec.Code, rec.Body.String())
	}
	if rec := get("Bearer wrong", "employee:11111111-1111-1111-1111-111111111111"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token=%d %s", rec.Code, rec.Body.String())
	}
	if rec := get("Bearer admin-dev-credential", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing actor=%d %s", rec.Code, rec.Body.String())
	}
	if rec := get("Bearer admin-dev-credential", "employee:11111111-1111-1111-1111-111111111111"); rec.Code != http.StatusNoContent {
		t.Fatalf("good=%d %s", rec.Code, rec.Body.String())
	}

	emptyToken := &handler{token: ""}
	emptyRouter := chi.NewRouter()
	emptyRouter.Use(emptyToken.authenticate)
	emptyRouter.Get("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer admin-dev-credential")
	req.Header.Set("X-Admin-Actor", "automation:sync")
	rec := httptest.NewRecorder()
	emptyRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty configured token=%d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("missing WWW-Authenticate")
	}
	if !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
		t.Fatalf("envelope=%s", rec.Body.String())
	}
}
