package leadstatus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// Seed the pre-module durable wire representation directly: compatibility must
// not depend on a new admission helper writing the same new format it reads.
func TestModuleResumesLegacyJobAndReplaysLegacyIdempotencyReceipt(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	principal := widgetPrincipal(t, pool, 601, 81)
	jobStore := jobs.NewStore(pool)
	legacyJob, err := jobStore.Enqueue(ctx, jobs.EnqueueParams{
		InstallationID: &principal.InstallationID,
		Type:           "workflow.lead.set_status", ActorType: "widget_user", ActorID: "81",
		ResourceType: "lead", ResourceID: "5001", Priority: 40, MaxAttempts: 5,
		Payload: json.RawMessage(`{"lead_id":5001,"pipeline_id":6001,"status_id":7001}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyHash := sha256.Sum256([]byte("legacy-request"))
	requestHash := sha256.Sum256([]byte(fmt.Sprintf("widget.lead.set_status:v1\x00%s\x00601\x0081\x00%s\x005001\x006001\x007001", principal.InstallationID, principal.ClientUUID)))
	receipt, _ := json.Marshal(map[string]any{"job_id": legacyJob.ID, "status": "queued"})
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys(installation_id,scope,key_hash,request_hash,status,job_id,response_status,response_body,expires_at)
 VALUES($1,'widget.lead.set_status:v1',$2,$3,'completed',$4,202,$5,now()+interval '1 hour')`, principal.InstallationID, keyHash[:], requestHash[:], legacyJob.ID, receipt); err != nil {
		t.Fatal(err)
	}

	module := NewModule(pool, jobStore)
	router := chi.NewRouter()
	module.RegisterHTTP(router, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), principal)))
		})
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/widget/actions/leads/set-status", bytes.NewBufferString(`{"lead_id":5001,"pipeline_id":6001,"status_id":7001}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "legacy-request")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("legacy action replay = %d %s, headers %v", response.Code, response.Body, response.Header())
	}
	var replay widgetapi.ActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil || replay.JobID != legacyJob.ID {
		t.Fatalf("legacy receipt = %+v, %v", replay, err)
	}

	remote := newLeadStatusRemote(5001, 1, 2)
	api, closeRemote := remote.client(principal)
	defer closeRemote()
	handlers := map[string]jobs.Handler{}
	observers := map[string]jobs.FailureObserver{}
	module.RegisterJobs(handlers, observers, api)
	for _, typ := range []string{"workflow.lead.set_status", "workflow.rule.lead_status.configure", "workflow.lead.status_transition"} {
		if handlers[typ] == nil {
			t.Fatalf("legacy job type %s was not registered", typ)
		}
	}
	if observers["workflow.lead.status_transition"] == nil {
		t.Fatal("legacy transition has no failure observer")
	}
	claimed, err := jobStore.Claim(ctx, "legacy-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim legacy job = %+v, %v", claimed, err)
	}
	result, err := handlers["workflow.lead.set_status"](ctx, claimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"lead_id":5001,"pipeline_id":6001,"status_id":7001,"converged":true}` {
		t.Fatalf("legacy result = %s", result)
	}
	remote.assertCounts(t, 2, 1, 1)
}
