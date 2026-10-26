package activitybridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserSessionIsHashedScopedExpiringAndRechecksAdmin(t *testing.T) {
	pool, p := bridgeDatabase(t)
	policy := &admissionPolicy{}
	b := New(pool, policy, nil, nil)
	b.ConfigureShare([]string{"https://panel.example.test"}, "server-only-secret")
	request := func(origin string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/widget/activity/browser-session", strings.NewReader(`{"app_origin":"`+origin+`"}`)).WithContext(widgetauth.ContextWithPrincipal(context.Background(), p))
	}
	req := request("https://attacker.invalid")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	b.BrowserSessionHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("untrusted origin status %d", rr.Code)
	}
	req = request("https://panel.example.test")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	b.BrowserSessionHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status %d %s", rr.Code, rr.Body)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.URL)
	if err != nil || u.RawQuery != "" || !strings.Contains(u.Fragment, "admin_session=") {
		t.Fatal("token leaked outside fragment")
	}
	q, _ := url.ParseQuery(strings.SplitN(u.Fragment, "?", 2)[1])
	token := q.Get("admin_session")
	hash := sha256.Sum256([]byte(token))
	var stored []byte
	if err := pool.QueryRow(context.Background(), `SELECT token_hash FROM activity_browser_sessions`).Scan(&stored); err != nil || !bytes.Equal(stored, hash[:]) {
		t.Fatal("session was not stored hashed")
	}
	auth, scope, err := b.browserManagementAuth(context.Background(), token)
	if err != nil || auth.Token == "" || scope.InstallationID != p.InstallationID || scope.IntegrationID != p.IntegrationID {
		t.Fatalf("wrong session scope %+v %v", scope, err)
	}
	policy.deny = true
	if _, _, err = b.browserManagementAuth(context.Background(), token); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal("revoked admin session was accepted")
	}
	policy.deny = false
	if _, err = pool.Exec(context.Background(), `UPDATE activity_browser_sessions SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.browserManagementAuth(context.Background(), token); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
		t.Fatal("expired session accepted")
	}
}
