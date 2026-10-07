package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type verifierFake struct{ p widgetauth.Principal }

func (v verifierFake) Verify(context.Context, string) (widgetauth.Principal, error) { return v.p, nil }
func TestHTTPBindingRecoveryMappingsAndCurrentPermissions(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	s := NewStore(pool)
	integration, install, company := uuid.New(), uuid.New(), uuid.New()
	key := "service"
	secret := strings.Repeat("z", 32)
	scope := Scope{key, company, install}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,'x','https://service.test/oauth')`, []any{integration, uuid.NewString(), uuid.NewString()}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'test.amocrm.ru','active')`, []any{install, integration}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)`, []any{integration}},
		{`INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES($1,$2,$3,true)`, []any{key, company, install}},
	} {
		if _, e := pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	p := widgetauth.Principal{IntegrationID: integration, InstallationID: install, AccountID: 123, UserID: 1, TokenID: uuid.NewString(), Issuer: "https://test.amocrm.ru", TokenRetainUntil: time.Now().Add(time.Minute)}
	f := &crmFake{users: map[int64]amocrm.DistributionUser{1: {ID: 1, Name: "Employee", Rights: amocrm.DistributionRights{IsActive: ptr(true), IsAdmin: true, Leads: map[string]string{"view": "A"}}}}, lead: amocrm.DistributionLead{ID: 10, PipelineID: 20, StatusID: 30, ResponsibleUserID: 1}}
	h := &Handler{Store: s, CRM: f, Verifier: verifierFake{p}}
	router := chi.NewRouter()
	h.RegisterService(router, Auth{Keys: map[string]string{key: secret}, Store: s})
	call := func(method, path string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		if body == nil {
			raw = nil
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		Sign(r, scope, secret, raw)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		if out.Code != want {
			t.Fatalf("%s %s got%d want%d body%s", method, path, out.Code, want, out.Body)
		}
		return out
	}
	b := Binding{ID: uuid.New(), CompanyID: company, InstallationID: install, IntegrationID: integration, AccountID: 123, Revision: 1, IntentID: uuid.New()}
	bindingBody := struct {
		Binding
		WidgetToken string    `json:"widgetToken"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}{b, "fake", time.Now().Add(10 * time.Minute)}
	call("POST", "/internal/v1/distribution/bindings", bindingBody, 201)
	call("POST", "/internal/v1/distribution/bindings", bindingBody, 200)
	var used int
	if e := pool.QueryRow(ctx, `SELECT count(*) FROM used_widget_tokens`).Scan(&used); e != nil || used != 1 {
		t.Fatal("JTI recovery consumed twice", used, e)
	}
	base := "/internal/v1/distribution/bindings/" + b.ID.String()
	employee := uuid.New()
	snapshot := map[string]any{"mappingRevision": 1, "mappings": []Mapping{{employee, 1}}}
	ack := call("PUT", base+"/mappings", snapshot, 200)
	if !strings.Contains(ack.Body.String(), `"mappingRevision":1`) {
		t.Fatal("invalidmappingack", ack.Body)
	}
	call("PUT", base+"/mappings", snapshot, 200)
	snapshot["mappings"] = []Mapping{}
	call("PUT", base+"/mappings", snapshot, 409)
	out := call("GET", base+"/references", nil, 200)
	if !strings.Contains(out.Body.String(), `"id":"1"`) || strings.Contains(out.Body.String(), "rights") || !strings.Contains(out.Body.String(), `"timezone":"UTC"`) || !strings.Contains(out.Body.String(), `"accountDomain":"test.amocrm.ru"`) {
		t.Fatal("referenceprojection", out.Body)
	}
	var refs struct {
		TimezoneFetchedAt time.Time `json:"timezoneFetchedAt"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &refs); err != nil || refs.TimezoneFetchedAt.IsZero() {
		t.Fatal("timezone freshness is missing", err, out.Body)
	}
	f.timezone = "Europe/Moscow"
	if out = call("GET", base+"/references", nil, 200); !strings.Contains(out.Body.String(), `"timezone":"Europe/Moscow"`) {
		t.Fatal("account timezone change was not refreshed", out.Body)
	}
	f.timezone = "Local"
	call("GET", base+"/references", nil, 503)
	f.timezone = "UTC"
	f.timezoneErr = ErrUnavailable
	call("GET", base+"/references", nil, 503)
	f.timezoneErr = nil
	if _, err := pool.Exec(ctx, "UPDATE installations SET account_domain='https://attacker.invalid/' WHERE id=$1", install); err != nil {
		t.Fatal(err)
	}
	call("GET", base+"/references", nil, 503)
	if _, err := pool.Exec(ctx, "UPDATE installations SET account_domain='TEST.AMOCRM.RU' WHERE id=$1", install); err != nil {
		t.Fatal(err)
	}
	if out = call("GET", base+"/references", nil, 200); !strings.Contains(out.Body.String(), `"accountDomain":"test.amocrm.ru"`) {
		t.Fatal("account domain was not canonicalized", out.Body)
	}
	permission := map[string]any{"employeeId": employee, "userId": "1", "leadId": "10"}
	out = call("POST", base+"/permissions", permission, 200)
	if !strings.Contains(out.Body.String(), `"canViewLead":true`) {
		t.Fatal(out.Body)
	}
	permission["userId"] = "2"
	call("POST", base+"/permissions", permission, 403)
	permission["userId"] = 1
	call("POST", base+"/permissions", permission, 400)
	call("POST", base+"/revoke", nil, 200)
	call("POST", base+"/permissions", map[string]any{"employeeId": employee, "userId": "1", "leadId": "10"}, 403)
	// Revoke an intent before confirm: late JWT approval cannot create binding.
	late := b
	late.ID = uuid.New()
	late.IntentID = uuid.New()
	call("POST", "/internal/v1/distribution/bindings/"+late.ID.String()+"/revoke", nil, 200)
	bindingBody.Binding = late
	call("POST", "/internal/v1/distribution/bindings", bindingBody, 409)
}
