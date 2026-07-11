package widgetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func TestPingHTTPContractAndReplay(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	principal := widgetPrincipal(t, pool, 101, 17)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/widget/actions/ping", nil)
	request.Header.Set("Idempotency-Key", "http-contract-key")
	request = request.WithContext(widgetauth.ContextWithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	handler.Ping(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("first response status/body = %d/%q", response.Code, response.Body.String())
	}
	var first ActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || first.JobID == uuid.Nil {
		t.Fatalf("first response = %q, error = %v", response.Body.String(), err)
	}

	fresh := principal
	fresh.TokenID = uuid.NewString()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/widget/actions/ping", nil)
	request.Header.Set("Idempotency-Key", "http-contract-key")
	request = request.WithContext(widgetauth.ContextWithPrincipal(request.Context(), fresh))
	response = httptest.NewRecorder()
	handler.Ping(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay response status/header/body = %d/%q/%q",
			response.Code, response.Header().Get("Idempotency-Replayed"), response.Body.String())
	}
	var replay ActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil || replay.JobID != first.JobID {
		t.Fatalf("replay response = %q, error = %v", response.Body.String(), err)
	}
	assertWidgetCounts(t, pool, 2, 1, 1)
}

func TestPingHTTPRejectsInvalidAdmissionBeforeConsumption(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	principal := widgetPrincipal(t, pool, 102, 19)

	tests := map[string]func(*http.Request){
		"missing idempotency key": func(*http.Request) {},
		"multiple idempotency keys": func(request *http.Request) {
			request.Header.Add("Idempotency-Key", "first")
			request.Header.Add("Idempotency-Key", "second")
		},
		"body is forbidden": func(request *http.Request) {
			request.Header.Set("Idempotency-Key", "body-request")
		},
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			var body *bytes.Reader
			if name == "body is forbidden" {
				body = bytes.NewReader([]byte(`{"account_id":999}`))
			} else {
				body = bytes.NewReader(nil)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/widget/actions/ping", body)
			configure(request)
			request = request.WithContext(widgetauth.ContextWithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()
			handler.Ping(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("response status/body = %d/%q", response.Code, response.Body.String())
			}
		})
	}
	assertWidgetCounts(t, pool, 0, 0, 0)
}

func TestLeadSetStatusHTTPAdmissionAndStrictBody(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	principal := widgetPrincipal(t, pool, 104, 25)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/widget/actions/leads/set-status",
		bytes.NewBufferString(`{"lead_id":501,"pipeline_id":601,"status_id":701}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lead-status-http")
	request = request.WithContext(widgetauth.ContextWithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	handler.LeadSetStatus(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("response status/body = %d/%q", response.Code, response.Body.String())
	}

	invalid := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/widget/actions/leads/set-status",
		bytes.NewBufferString(`{"lead_id":502,"pipeline_id":602,"status_id":702,"unexpected":true}`),
	)
	invalid.Header.Set("Content-Type", "application/json")
	invalid.Header.Set("Idempotency-Key", "lead-status-invalid")
	invalid = invalid.WithContext(widgetauth.ContextWithPrincipal(invalid.Context(), principal))
	invalidResponse := httptest.NewRecorder()
	handler.LeadSetStatus(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid response status/body = %d/%q", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestLeadStatusRuleHTTPAdmissionRequiresCompleteStrictCommand(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	principal := widgetPrincipal(t, pool, 106, 29)

	invalid := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/widget/workflow-rules/lead-status/configure",
		bytes.NewBufferString(`{"source_pipeline_id":1,"source_status_id":2,"target_pipeline_id":3,"target_status_id":4,"expected_revision":0}`),
	)
	invalid.Header.Set("Content-Type", "application/json")
	invalid.Header.Set("Idempotency-Key", "rule-invalid")
	invalid = invalid.WithContext(widgetauth.ContextWithPrincipal(invalid.Context(), principal))
	invalidResponse := httptest.NewRecorder()
	handler.ConfigureLeadStatusRule(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid response status/body = %d/%q", invalidResponse.Code, invalidResponse.Body.String())
	}

	valid := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/widget/workflow-rules/lead-status/configure",
		bytes.NewBufferString(`{"source_pipeline_id":1,"source_status_id":2,"target_pipeline_id":3,"target_status_id":4,"enabled":false,"expected_revision":0}`),
	)
	valid.Header.Set("Content-Type", "application/json")
	valid.Header.Set("Idempotency-Key", "rule-valid")
	valid = valid.WithContext(widgetauth.ContextWithPrincipal(valid.Context(), principal))
	validResponse := httptest.NewRecorder()
	handler.ConfigureLeadStatusRule(validResponse, valid)
	if validResponse.Code != http.StatusAccepted {
		t.Fatalf("valid response status/body = %d/%q", validResponse.Code, validResponse.Body.String())
	}
	var tokens, keys, ruleJobs int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM used_widget_tokens),
		(SELECT count(*) FROM idempotency_keys),
		(SELECT count(*) FROM jobs WHERE type=$1)`,
		LeadStatusRuleConfigureJobType).Scan(&tokens, &keys, &ruleJobs); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 || keys != 1 || ruleJobs != 1 {
		t.Fatalf("token/key/rule-job counts = %d/%d/%d", tokens, keys, ruleJobs)
	}
}

func TestJobStatusIsWidgetUserScoped(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	owner := widgetPrincipal(t, pool, 103, 23)
	action, err := handler.actions.EnqueuePing(context.Background(), owner, "owned-job")
	if err != nil {
		t.Fatal(err)
	}
	internalJob, err := jobStore.Enqueue(context.Background(), jobs.EnqueueParams{
		InstallationID: &owner.InstallationID,
		Type:           "webhook.parse",
		Payload:        map[string]any{"user_id": owner.UserID, "account_id": owner.AccountID},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertStatus := func(principal widgetauth.Principal, jobID uuid.UUID, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/jobs/"+jobID.String(), nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("jobID", jobID.String())
		ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
		ctx = widgetauth.ContextWithPrincipal(ctx, principal)
		response := httptest.NewRecorder()
		handler.JobStatus(response, request.WithContext(ctx))
		if response.Code != want {
			t.Fatalf("job %s response status/body = %d/%q, want %d",
				jobID, response.Code, response.Body.String(), want)
		}
	}
	assertStatus(owner, action.JobID, http.StatusOK)
	otherUser := owner
	otherUser.UserID++
	assertStatus(otherUser, action.JobID, http.StatusNotFound)
	assertStatus(owner, internalJob.ID, http.StatusNotFound)
}

func TestJobStatusRendersOnlyTypedWorkflowResult(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	owner := widgetPrincipal(t, pool, 105, 27)
	action, err := handler.actions.EnqueueLeadSetStatus(
		context.Background(), owner, "typed-result",
		LeadStatusCommand{LeadID: 801, PipelineID: 802, StatusID: 803},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE jobs
		SET status='completed', result=$2, finished_at=now()
		WHERE id=$1`, action.JobID,
		`{"lead_id":801,"pipeline_id":802,"status_id":803,"converged":true,"internal":"must-not-leak"}`,
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/jobs/"+action.JobID.String(), nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("jobID", action.JobID.String())
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = widgetauth.ContextWithPrincipal(ctx, owner)
	response := httptest.NewRecorder()
	handler.JobStatus(response, request.WithContext(ctx))
	if response.Code != http.StatusOK {
		t.Fatalf("response status/body = %d/%q", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte("internal")) || bytes.Contains(response.Body.Bytes(), []byte("must-not-leak")) {
		t.Fatalf("internal result leaked: %s", response.Body.String())
	}
}

func TestLeadStatusRuleJobStatusIsActorScopedAndTyped(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	jobStore := jobs.NewStore(pool)
	handler := NewHandler(jobStore, NewActionStore(pool, jobStore))
	owner := widgetPrincipal(t, pool, 107, 31)
	action, err := handler.actions.EnqueueLeadStatusRuleConfigure(
		context.Background(), owner, "typed-rule-result",
		LeadStatusRuleCommand{
			SourcePipelineID: 10, SourceStatusID: 20,
			TargetPipelineID: 30, TargetStatusID: 40,
			Enabled: true, ExpectedRevision: 0,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ruleID := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		UPDATE jobs
		SET status='completed', result=$2, finished_at=now()
		WHERE id=$1`, action.JobID,
		`{"rule_id":"`+ruleID.String()+`","source_pipeline_id":10,"source_status_id":20,"target_pipeline_id":30,"target_status_id":40,"enabled":true,"revision":1,"internal":"must-not-leak"}`,
	); err != nil {
		t.Fatal(err)
	}

	requestStatus := func(principal widgetauth.Principal) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/jobs/"+action.JobID.String(), nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("jobID", action.JobID.String())
		ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
		ctx = widgetauth.ContextWithPrincipal(ctx, principal)
		response := httptest.NewRecorder()
		handler.JobStatus(response, request.WithContext(ctx))
		return response
	}

	response := requestStatus(owner)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(ruleID.String())) ||
		bytes.Contains(response.Body.Bytes(), []byte("internal")) {
		t.Fatalf("owner response status/body = %d/%q", response.Code, response.Body.String())
	}
	otherUser := owner
	otherUser.UserID++
	if response := requestStatus(otherUser); response.Code != http.StatusNotFound {
		t.Fatalf("other user response status/body = %d/%q", response.Code, response.Body.String())
	}
}
