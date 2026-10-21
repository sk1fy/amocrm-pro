package taskcontrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testCRM struct {
	admin  bool
	active bool
}

func (c *testCRM) DistributionUser(_ context.Context, _ uuid.UUID, id int64) (amocrm.DistributionUser, error) {
	return amocrm.DistributionUser{ID: id, Rights: amocrm.DistributionRights{IsActive: &c.active, IsAdmin: c.admin}}, nil
}
func TestCoreJWTGrantReplayAndInstallationGeneration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	install, integration, company := uuid.New(), uuid.New(), uuid.New()
	client := uuid.NewString()
	secret := []byte("task-control-synthetic-client-secret")
	ring, e := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x31}, cryptox.KeySize)}, 1)
	if e != nil {
		t.Fatal(e)
	}
	encrypted, version, e := ring.Seal(secret, cryptox.IntegrationSecretAAD(integration))
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri) VALUES($1,'task-control-test',$2,$3,$4,'https://core.example.test/oauth/amocrm/callback')`, []any{integration, client, encrypted, version}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'task-control',true)`, []any{integration}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'tenant.amocrm.ru','active')`, []any{install, integration}},
		{`INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled) VALUES('service',$1,123,$2,true)`, []any{company, integration}},
	} {
		if _, e = pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	crm := &testCRM{admin: true, active: true}
	router, e := Router(ctx, pool, ring, crm, Config{Keys: map[string]string{"service": strings.Repeat("s", 32)}, AssertionKeyID: "signer", AssertionKey: key})
	if e != nil {
		t.Fatal(e)
	}
	token := func() string {
		now := time.Now()
		raw, e := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://tenant.amocrm.ru", "aud": "https://core.example.test", "jti": uuid.NewString(), "iat": now.Add(-time.Second).Unix(), "nbf": now.Add(-time.Second).Unix(), "exp": now.Add(30*time.Minute - time.Second).Unix(), "account_id": 123, "user_id": 7, "client_uuid": client}).SignedString(secret)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	call := func(raw, purpose string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"purpose": purpose, "requestId": uuid.NewString()})
		r := httptest.NewRequest("POST", "/api/v1/widget/task-control/auth", bytes.NewReader(body))
		r.Header.Set("X-Request-ID", uuid.NewString())
		r.Header.Set("X-Auth-Token", raw)
		r.Header.Set("Origin", "https://tenant.amocrm.ru")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Header().Get("X-Request-ID") != r.Header.Get("X-Request-ID") {
			t.Fatal("handoff request ID missing from listener response")
		}
		return w
	}
	raw := token()
	res := call(raw, "link")
	if res.Code != 200 {
		t.Fatalf("handoff %d %s", res.Code, res.Body.String())
	}
	var envelope struct {
		Assertion string `json:"assertion"`
	}
	if e = json.Unmarshal(res.Body.Bytes(), &envelope); e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(envelope.Assertion, ".")
	if len(parts) != 3 {
		t.Fatal("malformed assertion")
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(pub, []byte("teamos-task-control/v1\n"+parts[0]+"."+parts[1]), signature) {
		t.Fatal("invalid signature")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var a Assertion
	if e = json.Unmarshal(payload, &a); e != nil {
		t.Fatal(e)
	}
	if a.CompanyID != company || a.ActorID != "7" || !a.Admin || a.Generation != 1 || a.ExpiresAt-a.IssuedAt != 60 {
		t.Fatal(a)
	}
	if got := call(raw, "runtime"); got.Code != 403 {
		t.Fatalf("JWT replay %d", got.Code)
	}
	if got := call("broken", "runtime"); got.Code != 401 {
		t.Fatalf("invalid JWT %d", got.Code)
	}
	crm.admin = false
	if got := call(token(), "link"); got.Code != 403 {
		t.Fatalf("manager link %d", got.Code)
	}
	// Test manager directly through the verified principal path after limiter tests to avoid rate masking.
	crm.active = false
	if got := call(token(), "runtime"); got.Code != 403 {
		t.Fatalf("inactive manager %d", got.Code)
	}
	now := time.Now().UTC()
	reader := &Store{Pool: pool}
	st, e := reader.Read(ctx, "service", company, 123, uuid.New(), now)
	if e != nil || st.State != "installed" {
		t.Fatal(st, e)
	}
	nonce := uuid.New()
	if _, e = reader.Read(ctx, "service", company, 123, nonce, now); e != nil {
		t.Fatal(e)
	}
	if _, e = reader.Read(ctx, "service", company, 123, nonce, now); e != ErrDenied {
		t.Fatal("nonce replay", e)
	}
	if _, e = reader.Read(ctx, "service", uuid.New(), 123, uuid.New(), now); e != ErrDenied {
		t.Fatal("foreign company", e)
	}
	if _, e = pool.Exec(ctx, `UPDATE installations SET status='uninstalled' WHERE id=$1`, install); e != nil {
		t.Fatal(e)
	}
	st, e = reader.Read(ctx, "service", company, 123, uuid.New(), now)
	if e != nil || st.State != "notInstalled" {
		t.Fatal(st, e)
	}
	if _, e = pool.Exec(ctx, `UPDATE installations SET status='authorizing' WHERE id=$1`, install); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, install); e != nil {
		t.Fatal(e)
	}
	st, e = reader.Read(ctx, "service", company, 123, uuid.New(), now)
	if e != nil || st.Generation != 2 {
		t.Fatal("reinstall generation", st, e)
	}
	// Signed service route has separate credentials and rejects changed account/product.
	r := httptest.NewRequest(http.MethodGet, "/internal/v1/task-control/accounts/123/installation", nil)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	id := uuid.NewString()
	for k, v := range map[string]string{"Key-Id": "service", "Company": company.String(), "Account": "123", "Product": Product, "Timestamp": stamp, "Nonce": id, "Signature": Signature(strings.Repeat("s", 32), "service", "GET", r.URL.RequestURI(), company.String(), "123", stamp, id)} {
		r.Header.Set("X-Task-Control-"+k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	r.Header.Set("X-Task-Control-Product", "lead-distribution")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("product substitution", w.Code)
	}
	if _, e = pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1 AND service_code='task-control'`, integration); e != nil {
		t.Fatal(e)
	}
	st, e = reader.Read(ctx, "service", company, 123, uuid.New(), now)
	if e != nil || st.State != "unknown" {
		t.Fatal("disabled capability must not report installed", st, e)
	}
	if _, e = pool.Exec(ctx, `UPDATE task_control_service_grants SET enabled=false WHERE company_id=$1`, company); e != nil {
		t.Fatal(e)
	}
	if _, e = reader.Read(ctx, "service", company, 123, uuid.New(), now); e != ErrDenied {
		t.Fatal("revoked service grant accepted", e)
	}

}
