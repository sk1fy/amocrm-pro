package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type signedAssignmentHTTP struct {
	fixture *assignmentFixtureData
	router  http.Handler
	secret  string
}

func assignmentHTTP(t *testing.T, f *assignmentFixtureData) *signedAssignmentHTTP {
	t.Helper()
	secret := strings.Repeat("s", 32)
	h := &Handler{Store: f.Store, CRM: f.CRM}
	router := chi.NewRouter()
	h.RegisterService(router, Auth{Keys: map[string]string{f.Scope.KeyID: secret}, Store: f.Store})
	return &signedAssignmentHTTP{f, router, secret}
}
func (h *signedAssignmentHTTP) call(t *testing.T, scope Scope, method, path, key string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(raw))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if body != nil && want != 400 {
		name := "ReconcileInput"
		if path == "/internal/v1/distribution/assignments" {
			name = "AssignmentEnvelope"
		}
		validateRuntimeResponse(t, name, raw)
	}
	Sign(r, scope, h.secret, raw)
	out := httptest.NewRecorder()
	h.router.ServeHTTP(out, r)
	if out.Code != want {
		t.Fatalf("%s %s got%d want%d: %s", method, path, out.Code, want, out.Body.String())
	}
	schema := "ErrorResponse"
	if want == 202 {
		schema = "OperationReceipt"
	} else if want == 200 {
		schema = "Operation"
	}
	validateRuntimeResponse(t, schema, out.Body.Bytes())
	return out
}
func decodeHTTPAssignmentOperation(t *testing.T, out *httptest.ResponseRecorder) Operation {
	t.Helper()
	var op Operation
	if err := json.Unmarshal(out.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	return op
}
func TestSignedAssignmentHTTPAdmissionAndControlReceipts(t *testing.T) {
	f := assignmentFixture(t)
	h := assignmentHTTP(t, f)
	ctx := context.Background()
	key := uuid.NewString()
	path := "/internal/v1/distribution/assignments"
	h.call(t, f.Scope, "POST", path, key, f.Assignment, 202)
	replayed := h.call(t, f.Scope, "POST", path, key, f.Assignment, 202)
	if replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("missing admission replay marker")
	}
	changed := f.Assignment
	changed.Command.TargetResponsibleUserID = 3
	h.call(t, f.Scope, "POST", path, key, changed, 409)
	var count int
	if err := f.Pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE type=$1", AssignmentJobType).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate job %d %v", count, err)
	}
	operationPath := "/internal/v1/distribution/operations/" + f.Assignment.Command.OperationID.String()
	op := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "GET", operationPath, "", nil, 200))
	if op.OperationID != f.Assignment.Command.OperationID || op.State != "queued" {
		t.Fatalf("wrong operation %+v", op)
	}
	foreign := f.Scope
	foreign.CompanyID = uuid.New()
	h.call(t, foreign, "GET", operationPath, "", nil, 401)
	cancelKey := uuid.NewString()
	cancel := operationAction{ExpectedResultVersion: op.ResultVersion, Actor: Actor{Kind: "system"}, Reason: "authorized fixture cancellation"}
	cancelled := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", operationPath+"/cancel", cancelKey, cancel, 200))
	if cancelled.State != "cancelled" || cancelled.ExternalEffectState != "no_attempt" || !cancelled.ResolutionEvidence.GuardReleasable {
		t.Fatalf("cancel proof %+v", cancelled)
	}
	again := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", operationPath+"/cancel", cancelKey, cancel, 200))
	if again.ResultVersion != cancelled.ResultVersion {
		t.Fatal("cancel replay created another result")
	}
	cancel.Reason = "different actor intent"
	h.call(t, f.Scope, "POST", operationPath+"/cancel", cancelKey, cancel, 409)
	reconcileKey := uuid.NewString()
	reconcile := operationAction{ExpectedResultVersion: cancelled.ResultVersion, Actor: Actor{Kind: "system"}, Reason: "terminal recovery"}
	h.call(t, f.Scope, "POST", operationPath+"/reconcile", reconcileKey, reconcile, 200)
	h.call(t, f.Scope, "POST", operationPath+"/reconcile", reconcileKey, reconcile, 200)
	reconcile.Reason = "changed recovery payload"
	h.call(t, f.Scope, "POST", operationPath+"/reconcile", reconcileKey, reconcile, 409)
	if err := f.Pool.QueryRow(ctx, "SELECT count(*) FROM distribution_control_receipts WHERE operation_id=$1", op.OperationID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("control receipt count %d %v", count, err)
	}
	if err := f.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE object_type='distribution_operation' AND object_id=$1", op.OperationID.String()).Scan(&count); err != nil || count != 2 {
		t.Fatalf("control audit count %d %v", count, err)
	}

	second := f.Assignment
	second.MessageID = uuid.New()
	second.Command.OperationID = uuid.New()
	second.Command.DecisionID = uuid.New()
	second.Command.EpisodeID = uuid.New()
	second.Command.Expected.LeadID = 11
	h.call(t, f.Scope, "POST", path, uuid.NewString(), second, 202)
	secondPath := "/internal/v1/distribution/operations/" + second.Command.OperationID.String()
	reusedControl := operationAction{ExpectedResultVersion: 1, Actor: Actor{Kind: "system"}, Reason: "authorized fixture cancellation"}
	h.call(t, f.Scope, "POST", secondPath+"/cancel", cancelKey, reusedControl, 409)
	untouched := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "GET", secondPath, "", nil, 200))
	if untouched.State != "queued" {
		t.Fatal("cross-operation key silently cancelled another lead")
	}
	if _, err := f.Pool.Exec(ctx, "UPDATE integration_services SET enabled=false WHERE integration_id=$1 AND service_code='lead-distribution'", f.Assignment.Scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	h.call(t, f.Scope, "GET", operationPath, "", nil, 200)
	h.call(t, f.Scope, "POST", path, key, f.Assignment, 202)
	distinct := f.Assignment
	distinct.MessageID = uuid.New()
	distinct.Command.OperationID = uuid.New()
	distinct.Command.DecisionID = uuid.New()
	h.call(t, f.Scope, "POST", path, uuid.NewString(), distinct, 403)
}
func TestSignedAssignmentHTTPUnknownObservationNeverReleasesGuard(t *testing.T) {
	f := assignmentFixture(t)
	f.CRM.Mode = "timeout_applied"
	job := f.AdmitAndClaim(t)
	worker := f.Worker()
	if _, err := worker.Handler(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h := assignmentHTTP(t, f)
	path := "/internal/v1/distribution/operations/" + f.Assignment.Command.OperationID.String()
	op := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "GET", path, "", nil, 200))
	if op.State != "outcome_unknown" || op.ExternalEffectState != "unknown" || op.ResolutionEvidence.GuardReleasable || f.CRM.Calls != 1 {
		t.Fatalf("unknown outcome incorrectly settled %+v calls%d", op, f.CRM.Calls)
	}
	reconcile := operationAction{ExpectedResultVersion: op.ResultVersion, Actor: Actor{Kind: "system"}, Reason: "observe synthetic timeout"}
	key := uuid.NewString()
	observed := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", path+"/reconcile", key, reconcile, 200))
	if observed.State != "outcome_unknown" || observed.ResolutionEvidence.GuardReleasable {
		t.Fatalf("target observation falsely proved effect %+v", observed)
	}
	h.call(t, f.Scope, "POST", path+"/reconcile", key, reconcile, 200)
	cancel := operationAction{ExpectedResultVersion: observed.ResultVersion, Actor: Actor{Kind: "system"}, Reason: "cancel after uncertain dispatch"}
	cancelled := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", path+"/cancel", uuid.NewString(), cancel, 200))
	if cancelled.ResolutionEvidence.GuardReleasable || cancelled.State == "cancelled" {
		t.Fatalf("cancel discarded uncertain effect %+v", cancelled)
	}
	if _, err := worker.Handler(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if f.CRM.Calls != 1 {
		t.Fatal("recovery repeated PATCH")
	}
	next := f.Assignment
	next.MessageID = uuid.New()
	next.Command.OperationID = uuid.New()
	next.Command.DecisionID = uuid.New()
	next.Command.EpisodeID = uuid.New()
	h.call(t, f.Scope, "POST", "/internal/v1/distribution/assignments", uuid.NewString(), next, 409)
	// A different installation/binding for the same CRM account/lead still
	// cannot evade the unresolved operation's account-wide guard.
	ctx := context.Background()
	integration, installation, binding := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		query string
		args  []any
	}{{"UPDATE distribution_bindings SET state='revoked' WHERE id=$1", []any{f.Assignment.Scope.BindingID}}, {"INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,'x','https://fixture.invalid/oauth')", []any{integration, uuid.NewString(), uuid.NewString()}}, {"INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'test.amocrm.ru','active')", []any{installation, integration}}, {"INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)", []any{integration}}, {"INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,123,1,$5,1)", []any{binding, f.Scope.CompanyID, installation, integration, uuid.New()}}, {"INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES('service',$1,$2,true)", []any{f.Scope.CompanyID, installation}}} {
		if _, err := f.Pool.Exec(ctx, q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	next.Scope.BindingID = binding
	next.Scope.InstallationID = installation
	next.Scope.IntegrationID = integration
	newScope := f.Scope
	newScope.InstallationID = installation
	h.call(t, newScope, "POST", "/internal/v1/distribution/assignments", uuid.NewString(), next, 409)
	var guards int
	if err := f.Pool.QueryRow(ctx, "SELECT count(*) FROM distribution_lead_guards WHERE account_id=123 AND lead_id=10").Scan(&guards); err != nil || guards != 1 {
		t.Fatalf("unknown guard lost %d %v", guards, err)
	}
}
func TestAssignmentWorkerOrdinaryMappedActorUsesResourceRights(t *testing.T) {
	for _, view := range []string{"A", "D"} {
		t.Run(view, func(t *testing.T) {
			f := assignmentFixture(t)
			actorEmployee := uuid.New()
			if _, err := f.Pool.Exec(context.Background(), "INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,1)", f.Assignment.Scope.BindingID, actorEmployee); err != nil {
				t.Fatal(err)
			}
			crmActor := int64(1)
			f.Assignment.Command.Actor = Actor{Kind: "user", TeamOSUserID: &actorEmployee, CRMUserID: &crmActor}
			f.CRM.crmFake.lead = amocrm.DistributionLead{ID: 10, PipelineID: 20, StatusID: 30, ResponsibleUserID: 1}
			u := f.CRM.crmFake.users[1]
			u.Rights.IsAdmin = false
			u.Rights.Leads = map[string]string{"view": view}
			f.CRM.crmFake.users[1] = u
			job := f.AdmitAndClaim(t)
			if _, err := f.Worker().Handler(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			op, err := f.Store.Operation(context.Background(), f.Scope, f.Assignment.Command.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if view == "A" {
				if op.State != "succeeded" || f.CRM.Calls != 1 || !op.ResolutionEvidence.GuardReleasable {
					t.Fatalf("ordinary actor blocked %+v calls%d", op, f.CRM.Calls)
				}
			} else if op.State != "rejected" || f.CRM.Calls != 0 || op.ExternalEffectState != "no_attempt" {
				t.Fatalf("denied actor mutated %+v calls%d", op, f.CRM.Calls)
			}
		})
	}
}
