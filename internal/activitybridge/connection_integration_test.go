package activitybridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorConnectionScopeAndRevocation(t *testing.T) {
	pool, p := bridgeDatabase(t)
	if _, err := pool.Exec(context.Background(), `UPDATE integrations SET status='active' WHERE id=$1`, p.IntegrationID); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := corepolicy.New(pool, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = SetPilot(context.Background(), pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	b := New(pool, corepolicy.ForCaller(policy, "core"), &shareActivity{}, nil)
	b.ConfigureShare(nil, "test-operator-token")
	router := chi.NewRouter()
	b.RegisterShareHTTP(router)
	request := func(path, account, integration, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Activity-Installation-Id", p.InstallationID.String())
		r.Header.Set("X-Activity-Integration-Id", integration)
		r.Header.Set("X-Activity-Account-Id", account)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		return out
	}
	good := request("/api/v1/activity/connection", "42", p.IntegrationID.String(), "test-operator-token")
	if good.Code != 200 || !strings.Contains(good.Body.String(), `"account_id":"42"`) {
		t.Fatalf("scope=%d %s", good.Code, good.Body)
	}
	for _, path := range []string{"/api/v1/activity/connection", "/api/v1/activity/panels"} {
		if got := request(path, "43", p.IntegrationID.String(), "test-operator-token"); got.Code != 403 {
			t.Fatalf("account fence %s=%d", path, got.Code)
		}
		if got := request(path, "42", uuid.NewString(), "test-operator-token"); got.Code != 404 {
			t.Fatalf("integration fence %s=%d", path, got.Code)
		}
		if got := request(path, "42", p.IntegrationID.String(), "wrong-token"); got.Code != 401 {
			t.Fatalf("token %s=%d", path, got.Code)
		}
	}
	if err = SetPilot(context.Background(), pool, p.InstallationID, false); err != nil {
		t.Fatal(err)
	}
	if got := request("/api/v1/activity/connection", "42", p.IntegrationID.String(), "test-operator-token"); got.Code != 403 {
		t.Fatalf("revoked pilot=%d %s", got.Code, got.Body)
	}
}
