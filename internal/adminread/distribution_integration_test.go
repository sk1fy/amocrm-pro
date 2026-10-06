package adminread

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdminDistributionSummaryAndScopedChain(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integration := insertAdminIntegration(t, pool, "fixture-distribution-diagnostics")
	installation, other := uuid.New(), uuid.New()
	now := time.Now().UTC()
	insertAdminInstallation(t, pool, installation, integration, 91070707, "fixture-distribution.amocrm.test", "active", `{}`, now)
	insertAdminInstallation(t, pool, other, integration, 91080808, "fixture-foreign.amocrm.test", "active", `{}`, now)
	binding, company := uuid.New(), uuid.New()
	event, message, request, operation, decision, result, correlation, consumer, job := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	requestKey := uuid.New()
	requestHash := sha256.Sum256([]byte(requestKey.String()))
	statements := []struct {
		q string
		a []any
	}{
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)`, []any{integration}},
		{`INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,91070707,1,$5,1)`, []any{binding, company, installation, integration, uuid.New()}},
		{`INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES('fixture',$1,$2,true)`, []any{company, installation}},
		{`INSERT INTO webhook_consumer_receipts(id,event_id,installation_id,consumer_id,source_fingerprint,frozen_payload) VALUES($1,$2,$3,'core-lead-distribution-v1',decode(repeat('00',32),'hex'),'{}')`, []any{consumer, event, installation}},
		{`INSERT INTO jobs(id,installation_id,type,payload) VALUES($1,$2,'distribution.assign_responsible','{}')`, []any{job, installation}},
		{`INSERT INTO distribution_operations(id,binding_id,company_id,installation_id,integration_id,account_id,binding_revision,decision_id,lead_id,key_hash,request_hash,command,receipt,job_id,state,external_effect_state) VALUES($1,$2,$3,$4,$5,91070707,1,$6,99,$9,decode(repeat('00',32),'hex'),$7,'{}',$8,'outcome_unknown','unknown')`, []any{operation, binding, company, installation, integration, decision, mustJSON(t, map[string]any{"messageId": request, "eventId": event, "causationId": message, "correlationId": correlation}), job, requestHash[:]}},
		{`INSERT INTO distribution_event_outbox(message_id,consumer_receipt_id,scope,payload,state,attempts,error_code) VALUES($1,$2,$3,$4,'blocked',20,'invalid_ack')`, []any{message, consumer, mustJSON(t, map[string]any{"installationId": installation}), mustJSON(t, map[string]any{"eventId": event, "correlationId": correlation, "event": map[string]string{"leadId": "99"}, "secret_fixture_payload": "never return"})}},
		{`INSERT INTO distribution_operation_results(operation_id,result_version,payload) VALUES($1,1,'{}')`, []any{operation}},
		{`INSERT INTO distribution_result_outbox(operation_id,result_version,payload) VALUES($1,1,$2)`, []any{operation, mustJSON(t, map[string]any{"messageId": result})}},
	}
	for _, s := range statements {
		if _, e := pool.Exec(t.Context(), s.q, s.a...); e != nil {
			t.Fatal(e)
		}
	}
	router := adminTestRouter(t, pool)
	base := "/admin/v1/installations/" + installation.String() + "/distribution"
	response := adminGET(t, router, base)
	assertAdminResponseSchema(t, response.Code, response.Body.Bytes(), "DistributionSummary")
	var summary DistributionSummary
	decodeJSON(t, response, &summary)
	if !summary.ModuleEnabled || summary.Paused || summary.TeamQueueState != "unknown" || summary.Events.States["blocked"] != 1 || summary.Results.States["pending"] != 1 || summary.Events.OldestPendingAt == nil || summary.Operations["outcome_unknown"] != 1 {
		t.Fatal(summary)
	}
	for _, reference := range []uuid.UUID{event, message, request, requestKey, operation, decision, result, correlation, consumer} {
		response = adminGET(t, router, base+"/trace?reference="+reference.String())
		assertAdminResponseSchema(t, response.Code, response.Body.Bytes(), "DistributionTraceResponse")
		var out struct{ Items []DistributionTraceItem }
		decodeJSON(t, response, &out)
		found := map[string]bool{}
		for _, item := range out.Items {
			found[item.Kind] = true
		}
		for _, kind := range []string{"consumer", "event", "operation", "result"} {
			if !found[kind] {
				t.Fatalf("reference %s missing %s: %s", reference, kind, response.Body.String())
			}
		}
		foreign := adminGET(t, router, "/admin/v1/installations/"+other.String()+"/distribution/trace?reference="+reference.String())
		decodeJSON(t, foreign, &out)
		if len(out.Items) != 0 {
			t.Fatal("foreign chain leaked", foreign.Body.String())
		}
	}
	first := adminGET(t, router, base+"/trace?limit=1")
	var page struct {
		Items []DistributionTraceItem
		Next  *string `json:"next_cursor"`
	}
	decodeJSON(t, first, &page)
	if len(page.Items) != 1 || page.Next == nil {
		t.Fatal(first.Body.String())
	}
	firstID := page.Items[0].Kind + page.Items[0].ID
	second := adminGET(t, router, base+"/trace?limit=1&cursor="+*page.Next)
	decodeJSON(t, second, &page)
	if len(page.Items) != 1 || page.Items[0].Kind+page.Items[0].ID == firstID {
		t.Fatal("keyset duplicate", second.Body.String())
	}
	if _, e := pool.Exec(t.Context(), `UPDATE distribution_admin_pauses SET paused=true WHERE installation_id=$1`, installation); e != nil {
		t.Fatal(e)
	}
	response = adminGET(t, router, base)
	decodeJSON(t, response, &summary)
	if !summary.ModuleEnabled || !summary.Paused {
		t.Fatal("pause confused with capability", summary)
	}
	if response = adminGET(t, router, base+"/trace?reference=unsafe"); response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
	empty := adminGET(t, router, "/admin/v1/installations/"+other.String()+"/distribution")
	summary = DistributionSummary{}
	decodeJSON(t, empty, &summary)
	if summary.Binding != nil || summary.Events.OldestPendingAt != nil || len(summary.Events.States) != 0 {
		t.Fatal("empty falsely populated", empty.Body.String())
	}
	if len(summary.DigitalPipeline.Inbox.States) != 0 || summary.DigitalPipeline.Inbox.OldestPendingAt != nil {
		t.Fatal("empty digital pipeline inbox falsely populated", empty.Body.String())
	}
	if _, e := pool.Exec(t.Context(), `INSERT INTO distribution_dp_inbox(id,dedup_key,installation_id,account_id,lead_id,pipeline_id,status_id,event_type,direction,group_id,occurred_at,payload) VALUES($1,$2,$3,1,1,1,1,1,'went_to_trigger',$4,1,$5)`, uuid.New(), []byte("dp-diagnostic-key"), installation, uuid.New(), json.RawMessage("{}")); e != nil {
		t.Fatal(e)
	}
	response = adminGET(t, router, base)
	summary = DistributionSummary{}
	decodeJSON(t, response, &summary)
	assertAdminResponseSchema(t, response.Code, response.Body.Bytes(), "DistributionSummary")
	if summary.DigitalPipeline.Inbox.States["pending"] != 1 || summary.DigitalPipeline.Inbox.OldestPendingAt == nil {
		t.Fatal("digital pipeline inbox not surfaced", response.Body.String())
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func TestDistributionReadTimeoutIsUnavailableNotEmpty(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integration := insertAdminIntegration(t, pool, "fixture-timeout-distribution")
	id := uuid.New()
	insertAdminInstallation(t, pool, id, integration, 91919191, "fixture-timeout.amocrm.test", "active", `{}`, time.Now().UTC())
	router := chi.NewRouter()
	Register(router, Dependencies{Pool: pool, Timeout: time.Nanosecond, Token: testAdminToken})
	for _, path := range []string{"/admin/v1/installations/" + id.String() + "/distribution", "/admin/v1/installations/" + id.String() + "/distribution/trace"} {
		response := adminGET(t, router, path)
		if response.Code != 503 || !strings.Contains(response.Body.String(), "backend_unavailable") || strings.Contains(response.Body.String(), "\"items\"") {
			t.Fatal("timeout became empty observation", response.Code, response.Body.String())
		}
	}
}
