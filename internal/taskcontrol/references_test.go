package taskcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/crmreferences"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

type referenceCRMFunc func(context.Context, uuid.UUID, int64) (crmreferences.Data, error)

func (f referenceCRMFunc) CRMReferences(ctx context.Context, i uuid.UUID, a int64) (crmreferences.Data, error) {
	return f(ctx, i, a)
}
func emptyReferenceData() crmreferences.Data {
	return crmreferences.Data{Users: []crmreferences.User{}, Groups: []crmreferences.Group{}, Pipelines: []crmreferences.Pipeline{}, Customers: crmreferences.Customers{Availability: "unsupported", Statuses: []crmreferences.Status{}, CheckedAt: time.Now().UTC()}}
}
func signedReferenceRequest(company uuid.UUID) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/internal/task-control/accounts/123/references", nil)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := uuid.NewString()
	secret := strings.Repeat("s", 32)
	for k, v := range map[string]string{"Key-Id": "service", "Company": company.String(), "Account": "123", "Product": Product, "Timestamp": stamp, "Nonce": nonce, "Signature": Signature(secret, "service", r.Method, r.URL.RequestURI(), company.String(), "123", stamp, nonce)} {
		r.Header.Set("X-Task-Control-"+k, v)
	}
	return r
}
func TestReferencesDefaultDenyCacheAndLateGenerationFence(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	install, integration, company := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,'reference-test',$2,$3,'https://example.test/callback')`, integration, uuid.NewString(), []byte("synthetic-fixture"))
	exec(`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'task-control',true)`, integration)
	exec(`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'tenant.amocrm.ru','active')`, install, integration)
	exec(`INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled) VALUES('service',$1,123,$2,true)`, company, integration)
	store := &Store{Pool: pool}
	var calls atomic.Int32
	h := &ReferencesHandler{Store: store, Keys: map[string]string{"service": strings.Repeat("s", 32)}, Provider: &crmreferences.Provider{}, CRM: referenceCRMFunc(func(context.Context, uuid.UUID, int64) (crmreferences.Data, error) {
		calls.Add(1)
		return emptyReferenceData(), nil
	})}
	request := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(signedReferenceRequest(company)); w.Code != 403 {
		t.Fatal("old status grant acquired references", w.Code, w.Body.String())
	}
	exec(`UPDATE task_control_service_grants SET references_read=true WHERE company_id=$1`, company)
	r := signedReferenceRequest(company)
	replay := r.Clone(ctx)
	w := request(r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if request(replay).Code != 403 {
		t.Fatal("nonce replay accepted")
	}
	var result crmreferences.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scope.InstallationID != install.String() || result.Scope.Generation != 1 || !result.Complete {
		t.Fatal("wrong scope", result)
	}
	if w = request(signedReferenceRequest(company)); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("no cache", w.Code, calls.Load())
	}
	if w = request(signedReferenceRequest(uuid.New())); w.Code != 403 {
		t.Fatal("foreign company accepted")
	}
	exec(`UPDATE task_control_service_grants SET references_read=false WHERE company_id=$1`, company)
	if w = request(signedReferenceRequest(company)); w.Code != 403 {
		t.Fatal("cached revoke accepted")
	}
	exec(`UPDATE task_control_service_grants SET references_read=true WHERE company_id=$1`, company)
	h.CRM = referenceCRMFunc(func(context.Context, uuid.UUID, int64) (crmreferences.Data, error) {
		calls.Add(1)
		exec(`UPDATE installations SET status='uninstalled' WHERE id=$1`, install)
		exec(`UPDATE installations SET status='active' WHERE id=$1`, install)
		return emptyReferenceData(), nil
	})
	if w = request(signedReferenceRequest(company)); w.Code != 403 {
		t.Fatal("late reinstall result accepted", w.Code, w.Body.String())
	}
	h.CRM = referenceCRMFunc(func(context.Context, uuid.UUID, int64) (crmreferences.Data, error) {
		calls.Add(1)
		return emptyReferenceData(), nil
	})
	if w = request(signedReferenceRequest(company)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Scope.Generation != 2 {
		t.Fatal("reinstall generation lost", result.Scope)
	}
	// Domain A->B->A also fences old in-flight OAuth identity.
	exec(`UPDATE installations SET account_domain='other.amocrm.ru' WHERE id=$1`, install)
	exec(`UPDATE installations SET account_domain='tenant.amocrm.ru' WHERE id=$1`, install)
	if w = request(signedReferenceRequest(company)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Scope.Generation != 4 {
		t.Fatal("OAuth identity generation lost", result.Scope)
	}
	var originalClient string
	if err := pool.QueryRow(ctx, `SELECT client_id FROM integrations WHERE id=$1`, integration).Scan(&originalClient); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE integrations SET client_id=$1 WHERE id=$2`, uuid.NewString(), integration)
	exec(`UPDATE integrations SET client_id=$1 WHERE id=$2`, originalClient, integration)
	if w = request(signedReferenceRequest(company)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Scope.Generation != 6 {
		t.Fatal("OAuth client identity generation lost", result.Scope)
	}

}

type denyReferenceAuthorizer struct{ calls atomic.Int32 }

func (a *denyReferenceAuthorizer) AuthorizeReferences(context.Context, string, uuid.UUID, int64, *uuid.UUID, time.Time) (ReferenceIdentity, error) {
	a.calls.Add(1)
	return ReferenceIdentity{}, ErrDenied
}
func TestReferencesTransportRejectsAmbiguousOrUnsignedRequests(t *testing.T) {
	a := &denyReferenceAuthorizer{}
	h := &ReferencesHandler{Store: a, Keys: map[string]string{"service": strings.Repeat("s", 32)}}
	for _, kind := range []string{"query", "empty_query", "body", "chunked", "duplicate_header", "expired", "bad_signature", "product", "account", "path"} {
		t.Run(kind, func(t *testing.T) {
			r := signedReferenceRequest(uuid.New())
			switch kind {
			case "query":
				r.URL.RawQuery = "refresh=true"
			case "empty_query":
				r.URL.ForceQuery = true
			case "body":
				r.Body = http.NoBody
				r.ContentLength = 1
			case "chunked":
				r.TransferEncoding = []string{"chunked"}
				r.Body = http.NoBody
			case "duplicate_header":
				r.Header.Add("X-Task-Control-Company", uuid.NewString())
			case "expired":
				r.Header.Set("X-Task-Control-Timestamp", "1")
			case "bad_signature":
				r.Header.Set("X-Task-Control-Signature", strings.Repeat("0", 64))
			case "product":
				r.Header.Set("X-Task-Control-Product", "lead-distribution")
			case "account":
				r.Header.Set("X-Task-Control-Account", "999")
			case "path":
				r.URL.Path = "/internal/task-control/accounts/0123/references"
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatal("invalid request admitted", w.Code, w.Body.String())
			}
		})
	}
	r := signedReferenceRequest(uuid.New())
	r.Body = ioBody{bytes.NewReader([]byte("x"))}
	r.ContentLength = 0
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unknown-length body accepted")
	}
	if a.calls.Load() != 0 {
		t.Fatal("bad transport reached authorization")
	}
}

type ioBody struct{ *bytes.Reader }

func (ioBody) Close() error { return nil }
