package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWidgetRoutesRealAuthBindingAndSignedTeamOSPolicy(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	integration, install, company, binding, employee, client := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.NewString()
	secret := []byte("synthetic-widget-integration-secret")
	key := "server"
	serverSecret := strings.Repeat("s", 32)
	ring, e := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x42}, cryptox.KeySize)}, 1)
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
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri) VALUES($1,$2,$3,$4,$5,'https://backend.example.test/oauth/amocrm/callback')`, []any{integration, uuid.NewString(), client, encrypted, version}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'tenant.amocrm.ru','active')`, []any{install, integration}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)`, []any{integration}},
		{`INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,123,1,$5,99)`, []any{binding, company, install, integration, uuid.New()}},
		{`INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,1)`, []any{binding, employee}},
		{`INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES($1,$2,$3,true)`, []any{key, company, install}},
	} {
		if _, e := pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	var localAllowed atomic.Bool
	localAllowed.Store(true)
	var localUnavailable atomic.Bool
	var callbackCount atomic.Int32
	var runtimeCount atomic.Int32
	var revokeAfterDispatch atomic.Bool
	callback := httptest.NewTLSServer(Auth{Keys: map[string]string{key: serverSecret}, Store: NewStore(pool)}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbackCount.Add(1)
		if r.URL.Path == "/internal/v1/distribution/widget-runtime" {
			var in map[string]json.RawMessage
			if decode(r, &in) != nil {
				w.WriteHeader(400)
				return
			}
			var c, i, u string
			_ = json.Unmarshal(in["companyId"], &c)
			_ = json.Unmarshal(in["installationId"], &i)
			_ = json.Unmarshal(in["userId"], &u)
			if c != company.String() || i != install.String() || u != "1" {
				w.WriteHeader(403)
				return
			}
			runtimeCount.Add(1)
			if revokeAfterDispatch.Load() {
				localAllowed.Store(false)
			}
			write(w, 200, map[string]any{"items": []any{map[string]any{"id": "fixture"}}})
			return
		}
		if r.URL.Path != "/internal/v1/distribution/widget-access" {
			t.Error("unexpected callback", r.URL)
		}
		var claim struct {
			CompanyID       uuid.UUID `json:"companyId"`
			BindingID       uuid.UUID `json:"bindingId"`
			BindingRevision int64     `json:"bindingRevision"`
			InstallationID  uuid.UUID `json:"installationId"`
			IntegrationID   uuid.UUID `json:"integrationId"`
			AccountID       int64     `json:"accountId,string"`
			UserID          int64     `json:"userId,string"`
			LeadID          *int64    `json:"leadId,string"`
		}
		if decode(r, &claim) != nil || claim.CompanyID != company || claim.InstallationID != install || claim.BindingID != binding || claim.AccountID != 123 || claim.UserID != 1 {
			t.Error("invalidverifiedcallbackscope")
			w.WriteHeader(403)
			return
		}
		if localUnavailable.Load() {
			w.WriteHeader(503)
			return
		}
		write(w, 200, map[string]any{"allowed": localAllowed.Load(), "employeeId": employee, "reason": "current_team_os_policy"})
	})))
	defer callback.Close()
	f := &crmFake{users: map[int64]amocrm.DistributionUser{1: {ID: 1, Rights: amocrm.DistributionRights{IsActive: ptr(true), IsAdmin: false, Leads: map[string]string{"view": "A"}}}}, lead: amocrm.DistributionLead{ID: 10, PipelineID: 20, StatusID: 30, ResponsibleUserID: 1}}
	cfg := Config{Keys: map[string]string{key: serverSecret}, TeamOSURL: callback.URL, TeamOSKeyID: key, JWTLeeway: 5 * time.Second, JWTMaxLifetime: 15 * time.Minute}
	router, e := routerWithHTTPClient(ctx, pool, ring, f, cfg, callback.Client())
	if e != nil {
		t.Fatal(e)
	}
	sign := func() string {
		t.Helper()
		now := time.Now()
		token, e := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://tenant.amocrm.ru", "aud": "https://backend.example.test", "jti": uuid.NewString(), "iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "account_id": int64(123), "user_id": int64(1), "client_uuid": client}).SignedString(secret)
		if e != nil {
			t.Fatal(e)
		}
		return token
	}
	runtimeBody := []byte(`{"kind":"groups"}`)
	call := func(method, path, origin, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		// These are authentication assertions; space requests beyond the production 5rps refill.
		time.Sleep(220 * time.Millisecond)
		raw := []byte{}
		if method == "POST" {
			raw = []byte(`{"leadId":"10"}`)
			if strings.HasSuffix(path, "/runtime") {
				raw = runtimeBody
			}
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("X-Auth-Token", token)
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		if out.Code != want {
			t.Fatalf("%s %s got%d want%d body%s", method, path, out.Code, want, out.Body)
		}
		return out
	}
	origin := "https://tenant.amocrm.ru"
	bootstrap := "/api/v1/widget/distribution/bootstrap"
	permission := "/api/v1/widget/distribution/permissions"
	token := sign()
	out := call("GET", bootstrap, origin, token, 200)
	if !strings.Contains(out.Body.String(), binding.String()) {
		t.Fatal("missingbinding", out.Body)
	}
	call("GET", bootstrap, origin, token, 401)
	out = call("POST", permission, origin, sign(), 200)
	if !strings.Contains(out.Body.String(), `"canViewLead":true`) {
		t.Fatal("ordinaryemployee denied", out.Body)
	}
	// Wrong origin rejects before token consumption or TeamOS policy lookup.
	before := callbackCount.Load()
	token = sign()
	call("GET", bootstrap, "https://foreign.amocrm.ru", token, 403)
	if callbackCount.Load() != before {
		t.Fatal("foreignorigincalledpolicy")
	}
	call("GET", bootstrap, origin, token, 200)
	localAllowed.Store(false)
	out = call("POST", permission, origin, sign(), 200)
	if !strings.Contains(out.Body.String(), `"canViewLead":false`) {
		t.Fatal("localdenygrants", out.Body)
	}
	localUnavailable.Store(true)
	call("GET", bootstrap, origin, sign(), 503)
	localUnavailable.Store(false)
	localAllowed.Store(true)
	runtimePath := "/api/v1/widget/distribution/runtime"
	out = call("POST", runtimePath, origin, sign(), 200)
	if !strings.Contains(out.Body.String(), "fixture") {
		t.Fatal("runtime bridge missing")
	}
	runtimeBody = []byte(`{"kind":"groups","companyId":"forged"}`)
	beforeRuntime := runtimeCount.Load()
	call("POST", runtimePath, origin, sign(), 400)
	if runtimeCount.Load() != beforeRuntime {
		t.Fatal("browser scope reached TeamOS")
	}
	runtimeBody = []byte(`{"kind":"rule","write":true,"requestId":"` + uuid.NewString() + `","payload":{"expectedRevision":1,"active":false,"keepCurrentResponsible":true}}`)
	call("POST", runtimePath, origin, sign(), 403)
	user := f.users[1]
	user.Rights.IsAdmin = true
	f.users[1] = user
	call("POST", runtimePath, origin, sign(), 200)
	revokeAfterDispatch.Store(true)
	out = call("POST", runtimePath, origin, sign(), 503)
	if !strings.Contains(out.Body.String(), "outcome_unknown") {
		t.Fatal("postdispatch denial falsely definite", out.Body)
	}
	revokeAfterDispatch.Store(false)
	localAllowed.Store(true)
	runtimeBody = []byte(`{"kind":"lead","leadId":"10"}`)
	user.Rights.IsAdmin = false
	user.Rights.Leads = map[string]string{"view": "D"}
	f.users[1] = user
	beforeRuntime = runtimeCount.Load()
	call("POST", runtimePath, origin, sign(), 403)
	if runtimeCount.Load() != beforeRuntime {
		t.Fatal("CRM denied lead reached private runtime")
	}
	user.Rights.Leads = map[string]string{"view": "A"}
	f.users[1] = user
	call("POST", runtimePath, origin, sign(), 200)
	if _, e := pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1 AND service_code='lead-distribution'`, integration); e != nil {
		t.Fatal(e)
	}
	call("POST", permission, origin, sign(), 403)
	// Replies never contain the OAuth/client secret, widget token or full rights.
	if strings.Contains(out.Body.String(), string(secret)) || strings.Contains(out.Body.String(), "rights") {
		t.Fatal("unsafeprojection")
	}
}
