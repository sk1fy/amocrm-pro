package adminread

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestTaskControlDiagnosticsRequiresAdmin(t *testing.T) {
	router := chi.NewRouter()
	Register(router, Dependencies{Token: "admin-secret"})
	for _, headers := range []map[string]string{nil, {"Authorization": "Bearer runtime-session", "X-Admin-Actor": "employee:synthetic"}, {"Authorization": "Bearer admin-secret"}} {
		req := httptest.NewRequest("GET", "/admin/v1/installations/"+uuid.NewString()+"/task-control/diagnostics", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("unauthorized=%d", rec.Code)
		}
	}
}

func TestTaskControlDiagnosticsSafeGenerationAndLifecycle(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integration := insertAdminIntegration(t, pool, "task-control-diagnostics")
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'task-control',true)`, integration); err != nil {
		t.Fatal(err)
	}
	install := uuid.New()
	insertAdminInstallation(t, pool, install, integration, 98765432, "private-subdomain.amocrm.test", "active", `{"secret":"canary-rule-secret","rules":[{"uid":"private-rule"}]}`, time.Now().UTC())
	company := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled) VALUES('canary-key-secret',$1,98765432,$2,true)`, company, integration); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_control_runtime_descriptors(installation_id,generation,runtime_version,confirmed_by) VALUES($1,1,'1.2.2','private-operator')`, install); err != nil {
		t.Fatal(err)
	}
	router := adminTestRouter(t, pool)
	read := func(code string) TaskControlDiagnostics {
		rec := adminGET(t, router, "/admin/v1/installations/"+install.String()+"/task-control/diagnostics")
		if rec.Code != http.StatusOK {
			t.Fatal(rec.Code, rec.Body.String())
		}
		assertAdminResponseSchema(t, rec.Code, rec.Body.Bytes(), "TaskControlDiagnostics")
		for _, secret := range []string{"canary-", "private-", company.String(), `"rules"`, `"actor"`, `"assertion"`, `"token"`} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatal("secret projection", rec.Body.String())
			}
		}
		var out TaskControlDiagnostics
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Code != code || out.RequestID == uuid.Nil || rec.Header().Get("X-Request-ID") != out.RequestID.String() || rec.Header().Get("Cache-Control") != "no-store" || out.FreshUntil.Sub(out.ObservedAt) != 5*time.Minute {
			t.Fatal(out, rec.Header())
		}
		return out
	}
	if out := read("ready"); out.RuntimeVersion != "1.2.2" || !out.RuntimeCompatible || !out.DynamicRulesV2 {
		t.Fatal(out)
	}
	for _, version := range []string{"1.1.4", "1.2.0", "1.2.1", "1.2.2", "1.2.3"} {
		if _, err := pool.Exec(ctx, `UPDATE task_control_runtime_descriptors SET runtime_version=$2 WHERE installation_id=$1`, install, version); err != nil {
			t.Fatal(err)
		}
		out := read("ready")
		if out.RuntimeVersion != version || !out.RuntimeCompatible || out.DynamicRulesV2 != (version != "1.1.4") {
			t.Fatal("supported package misclassified", out)
		}
	}
	// No heartbeat or runtime telemetry exists in Core; registry stays active.
	if out := read("ready"); out.InstallationState != "active" {
		t.Fatal(out)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_control_installation_versions SET evidence='private-provider-error-token' WHERE installation_id=$1`, install); err != nil {
		t.Fatal(err)
	}
	if out := read("runtime_unconfirmed"); out.EvidenceCode != "unknown" || out.RuntimeCompatible {
		t.Fatal(out)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_control_installation_versions SET signal_pending=true,evidence='disconnect_unverified' WHERE installation_id=$1`, install); err != nil {
		t.Fatal(err)
	}
	if out := read("disconnect_unverified"); out.RuntimeCompatible || out.RuntimeVersion != "" || out.NextCheckAt == nil || out.InstallationState != "active" {
		t.Fatal(out)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_control_installation_versions SET signal_pending=false,evidence='registry',generation=2 WHERE installation_id=$1`, install); err != nil {
		t.Fatal(err)
	}
	if out := read("runtime_unconfirmed"); out.RuntimeVersion != "" || out.Generation != 2 {
		t.Fatal(out)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_control_service_grants SET enabled=false WHERE company_id=$1`, company); err != nil {
		t.Fatal(err)
	}
	read("grant_disabled")
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='uninstalled' WHERE id=$1`, install); err != nil {
		t.Fatal(err)
	}
	read("operator_uninstall")
	for _, path := range []string{"/admin/v1/installations/" + install.String() + "/task-control/diagnostics?secret=canary", "/admin/v1/installations/invalid/task-control/diagnostics"} {
		rec := adminGET(t, router, path)
		if rec.Code != 400 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	rec := adminGET(t, router, "/admin/v1/installations/"+uuid.NewString()+"/task-control/diagnostics")
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}
