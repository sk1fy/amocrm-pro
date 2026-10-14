package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"net/http/httptest"
	"testing"
)

func TestSignedPermissionBatchScopeRevocationAndPresentation(t *testing.T) {
	f := assignmentFixture(t)
	var employee uuid.UUID
	if err := f.Pool.QueryRow(context.Background(), "SELECT employee_id FROM distribution_actor_mappings WHERE binding_id=$1", f.Assignment.Scope.BindingID).Scan(&employee); err != nil {
		t.Fatal(err)
	}
	crm := &batchPolicyCRM{installation: f.Scope.InstallationID, crmFake: f.CRM.crmFake}
	h := &Handler{Store: f.Store, CRM: crm}
	router := chi.NewRouter()
	secret := "12345678901234567890123456789012"
	h.RegisterService(router, Auth{Keys: map[string]string{f.Scope.KeyID: secret}, Store: f.Store})
	path := "/internal/v1/distribution/bindings/" + f.Assignment.Scope.BindingID.String() + "/permissions/batch?bindingRevision=1"
	call := func(scope Scope, ids []string, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"employeeId": employee, "userId": "2", "leadIds": ids})
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		Sign(r, scope, secret, raw)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		if out.Code != want {
			t.Fatalf("status=%d want=%d %s", out.Code, want, out.Body)
		}
		return out.Body.Bytes()
	}
	for _, signed := range []bool{false, true} {
		raw, _ := json.Marshal(map[string]any{"employeeId": employee, "userId": "2", "leadIds": []string{"10"}})
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		if signed {
			Sign(r, f.Scope, secret, raw)
			r.Header.Set("X-Distribution-Signature", "bad")
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		if out.Code != 401 {
			t.Fatal("invalid signature accepted", out.Code)
		}
	}
	raw := call(f.Scope, []string{"10", "11"}, 200)
	validateRuntimeResponse(t, "BatchPermissionResult", raw)
	var out struct {
		Items []struct {
			LeadID            string
			CanViewLead       bool
			ResponsibleUserID *string
			LeadURL           *string
		}
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Items) != 2 || !out.Items[0].CanViewLead || out.Items[0].LeadURL == nil || *out.Items[0].LeadURL != "https://test.amocrm.ru/leads/detail/10" || out.Items[0].ResponsibleUserID == nil {
		t.Fatal(string(raw))
	}
	call(f.Scope, []string{"10", "10"}, 400)
	call(f.Scope, []string{"010"}, 400)
	foreign := f.Scope
	foreign.CompanyID = uuid.New()
	call(foreign, []string{"10"}, 401)
	crm.missing = true
	call(f.Scope, []string{"10", "11"}, 503)
	crm.missing = false
	user := crm.users[2]
	user.Rights.Leads = map[string]string{"view": "D"}
	crm.users[2] = user
	raw = call(f.Scope, []string{"10"}, 200)
	out.Items = nil
	_ = json.Unmarshal(raw, &out)
	if out.Items[0].CanViewLead || out.Items[0].LeadURL != nil || out.Items[0].ResponsibleUserID != nil {
		t.Fatal("denied row disclosed CRM data", string(raw))
	}

	user.Rights.Leads = map[string]string{"view": "A"}
	crm.users[2] = user
	crm.hook = func() {
		if _, err := f.Pool.Exec(context.Background(), "DELETE FROM distribution_actor_mappings WHERE binding_id=$1", f.Assignment.Scope.BindingID); err != nil {
			t.Error(err)
		}
	}
	call(f.Scope, []string{"10"}, 403)
	crm.hook = nil
	if _, err := f.Pool.Exec(context.Background(), "INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,2)", f.Assignment.Scope.BindingID, employee); err != nil {
		t.Fatal(err)
	}
	crm.hook = func() {
		if _, err := f.Pool.Exec(context.Background(), "UPDATE distribution_bindings SET state='revoked' WHERE id=$1", f.Assignment.Scope.BindingID); err != nil {
			t.Error(err)
		}
	}
	call(f.Scope, []string{"10"}, 403)
}
