package companyentry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestCompanyEntryHTTPRealJWTLiveCRMAndDurableReplay(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE teamos_account_companies`); err != nil {
		t.Fatal(err)
	}
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{42}, cryptox.KeySize)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	integration, installation, client := uuid.New(), uuid.New(), uuid.NewString()
	secret := []byte("company-entry-test-integration-secret")
	ciphertext, version, err := keys.Seal(secret, cryptox.IntegrationSecretAAD(integration))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri) VALUES($1,'entry-http',$2,$3,$4,'https://backend.example.test/oauth/amocrm/callback')`, integration, client, ciphertext, version)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,42,'tenant.amocrm.ru','active')`, installation, integration)
	if err != nil {
		t.Fatal(err)
	}
	_, _, crm, _ := testHandler()
	company, user := uuid.New(), uuid.New()
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Service "+strings.Repeat("p", 32) {
			t.Error("missing service auth")
			w.WriteHeader(401)
			return
		}
		var in EntryInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.ActorID != "7" || in.AccountID != "42" || in.IntegrationID != integration || in.InstallationID != installation {
			t.Error("principal mismatch", in)
		}
		calls++
		_ = json.NewEncoder(w).Encode(Entry{CompanyID: company, UserID: user, Code: strings.Repeat("c", 43), ExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer upstream.Close()
	runctx, cancel := context.WithCancel(ctx)
	defer cancel()
	router, err := Router(runctx, pool, keys, crm, Config{TeamOSURL: upstream.URL, TeamOSToken: strings.Repeat("p", 32), FrontendOrigin: "https://teamos.example.test", SubjectCheckKey: strings.Repeat("s", 32)})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(actor, account int64) string {
		t.Helper()
		now := time.Now()
		claims := jwt.MapClaims{"iss": "https://tenant.amocrm.ru", "aud": "https://backend.example.test", "jti": uuid.NewString(), "iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(), "account_id": account, "user_id": actor, "client_uuid": client}
		token, e := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
		if e != nil {
			t.Fatal(e)
		}
		return token
	}
	request := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/widget/company/entries", strings.NewReader(body))
		r.Header.Set("X-Auth-Token", token)
		r.Header.Set("Origin", "https://tenant.amocrm.ru")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	token := sign(7, 42)
	w := request(token, `{"intent":"create"}`)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	w = request(token, `{"intent":"create"}`)
	if w.Code != 401 || calls != 1 {
		t.Fatal("JWT replay accepted", w.Code, calls)
	}
	w = request(sign(7, 43), `{"intent":"create"}`)
	if w.Code != 401 {
		t.Fatal("cross account accepted", w.Code)
	}
	crm.profile.Rights.IsAdmin = false
	w = request(sign(7, 42), `{"intent":"enter"}`)
	if w.Code != 403 || calls != 1 {
		t.Fatal("revoked rights accepted", w.Code, calls)
	}
	crm.profile.Rights.IsAdmin = true
	// The durable observation cannot be overwritten even after an upstream commit.
	_, err = pool.Exec(ctx, `UPDATE teamos_account_companies SET company_id=$1 WHERE account_id=42`, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	w = request(sign(7, 42), `{"intent":"enter"}`)
	if w.Code != 409 || strings.Contains(w.Body.String(), "#code=") {
		t.Fatal("conflicting mapping exposed handoff", w.Code, w.Body.String())
	}
	_, err = pool.Exec(ctx, `UPDATE installations SET status='disabled' WHERE id=$1`, installation)
	if err != nil {
		t.Fatal(err)
	}

	callback := httptest.NewRequest("POST", "/internal/v1/company-entry/subject-check", strings.NewReader(fmt.Sprintf(`{"accountId":"42","actorId":"7","integrationId":"%s","installationId":"%s"}`, integration, installation)))
	callback.Header.Set("Authorization", "Service "+strings.Repeat("s", 32))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, callback)
	if w.Code != 403 {
		t.Fatal("revoked installation accepted", w.Code, w.Body.String())
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM used_widget_tokens`).Scan(&count); err != nil || count != 3 {
		t.Fatal("durable JTI count", count, err)
	}
}
func TestAccountCompanyAssociationConcurrentNoOverwrite(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if _, e := pool.Exec(ctx, `TRUNCATE teamos_account_companies`); e != nil {
		t.Fatal(e)
	}
	s := &Store{Pool: pool}
	first, second := uuid.New(), uuid.New()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, company := range []uuid.UUID{first, second} {
		wg.Add(1)
		go func(id uuid.UUID) { defer wg.Done(); results <- s.Associate(ctx, 42, id) }(company)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if err == ErrConflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	actual, err := s.Associated(ctx, 42)
	if err != nil || actual != first && actual != second {
		t.Fatal(actual, err)
	}
	if err = s.Associate(ctx, 43, actual); err == nil {
		t.Fatal("cross-account company association accepted")
	}
}
