package widgetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func TestLeadSetStatusWorkflowCompareBeforeWrite(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 120, 44)
	command := LeadStatusCommand{LeadID: 5001, PipelineID: 6001, StatusID: 7001}
	job := admitAndClaimLeadStatus(t, pool, principal, command)
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	api, closeRemote := remote.client(principal)
	defer closeRemote()

	handler := LeadSetStatusJobHandler(NewExecutionStore(pool), api)
	result, err := handler(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LeadStatusResult
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != (LeadStatusResult{LeadID: command.LeadID, PipelineID: command.PipelineID, StatusID: command.StatusID, Converged: true}) {
		t.Fatalf("result = %+v", decoded)
	}
	if _, err := handler(context.Background(), job); err != nil {
		t.Fatalf("duplicate handler execution: %v", err)
	}
	remote.assertCounts(t, 3, 2, 1)
	var action, objectID, outcome string
	var auditCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT min(action), min(object_id), min(metadata->>'outcome'), count(*)
		FROM audit_log WHERE installation_id=$1`, principal.InstallationID,
	).Scan(&action, &objectID, &outcome, &auditCount); err != nil {
		t.Fatal(err)
	}
	if action != LeadSetStatusJobType || objectID != "5001" || outcome != "converged" || auditCount != 1 {
		t.Fatalf("audit = %s/%s/%s count=%d", action, objectID, outcome, auditCount)
	}
}

func TestLeadSetStatusWorkflowRetryObservesAppliedState(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 121, 45)
	command := LeadStatusCommand{LeadID: 5002, PipelineID: 6002, StatusID: 7002}
	job := admitAndClaimLeadStatus(t, pool, principal, command)
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	remote.failAfterFirstPatch = true
	api, closeRemote := remote.client(principal)
	defer closeRemote()
	handler := LeadSetStatusJobHandler(NewExecutionStore(pool), api)

	_, firstErr := handler(context.Background(), job)
	if firstErr == nil || !jobs.Classify(firstErr, 1).Retryable {
		t.Fatalf("first uncertain PATCH error = %v", firstErr)
	}
	var firstEffectState string
	if err := pool.QueryRow(context.Background(), `
		SELECT state FROM outbound_effects WHERE correlation_job_id=$1`, job.ID,
	).Scan(&firstEffectState); err != nil || firstEffectState != "uncertain" {
		t.Fatalf("first effect state/error = %q/%v", firstEffectState, err)
	}
	jobStore := jobs.NewStore(pool)
	workerID := *job.LockedBy
	if status, err := jobStore.Fail(
		context.Background(), job, workerID, jobs.Classify(firstErr, job.Attempts), time.Millisecond,
	); err != nil || status != jobs.StatusRetry {
		t.Fatalf("fail first attempt status/error = %s/%v", status, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET run_after=now() WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := jobStore.Claim(context.Background(), "workflow-test-retry", 1, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaimed jobs/error = %+v/%v", reclaimed, err)
	}
	result, err := handler(context.Background(), reclaimed[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded LeadStatusResult
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Converged {
		t.Fatalf("retry result = %+v, want converged state", decoded)
	}
	remote.assertCounts(t, 3, 2, 1)
	var effectState string
	var effectCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT min(state), count(*) FROM outbound_effects WHERE correlation_job_id=$1`, job.ID,
	).Scan(&effectState, &effectCount); err != nil || effectState != "applied" || effectCount != 1 {
		t.Fatalf("effect state/count/error = %q/%d/%v", effectState, effectCount, err)
	}
	var auditCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_log WHERE correlation_job_id=$1`, job.ID,
	).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("correlated audit count/error = %d/%v", auditCount, err)
	}
}

func TestLeadSetStatusWorkflowRechecksTenantBeforePatch(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 122, 46)
	command := LeadStatusCommand{LeadID: 5003, PipelineID: 6003, StatusID: 7003}
	job := admitAndClaimLeadStatus(t, pool, principal, command)
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	remote.afterLeadRead = func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE installations SET status='disabled' WHERE id=$1`, principal.InstallationID); err != nil {
			t.Errorf("disable installation: %v", err)
		}
	}
	api, closeRemote := remote.client(principal)
	defer closeRemote()

	_, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job)
	failure := jobs.Classify(err, 1)
	if err == nil || failure.Code != "action_not_authorized" || failure.Retryable {
		t.Fatalf("disabled-before-patch error/failure = %v/%+v", err, failure)
	}
	remote.assertCounts(t, 2, 1, 0)
}

func TestLeadSetStatusWorkflowRechecksAdminBeforePatch(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 125, 49)
	command := LeadStatusCommand{LeadID: 5006, PipelineID: 6006, StatusID: 7006}
	job := admitAndClaimLeadStatus(t, pool, principal, command)
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	remote.afterLeadRead = func() { remote.admin = false }
	api, closeRemote := remote.client(principal)
	defer closeRemote()

	_, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job)
	if failure := jobs.Classify(err, 1); err == nil || failure.Code != "actor_forbidden" || failure.Retryable {
		t.Fatalf("revoked admin error/failure = %v/%+v", err, failure)
	}
	remote.assertCounts(t, 2, 1, 0)
}

func TestLeadSetStatusWorkflowLinearizesConcurrentDisable(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 127, 51)
	command := LeadStatusCommand{LeadID: 5008, PipelineID: 6008, StatusID: 7008}
	job := admitAndClaimLeadStatus(t, pool, principal, command)
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	disableStarted := make(chan struct{})
	disableDone := make(chan error, 1)
	remote.beforePatchResponse = func() {
		go func() {
			close(disableStarted)
			_, err := pool.Exec(context.Background(), `
				UPDATE installations SET status='disabled' WHERE id=$1`, principal.InstallationID)
			disableDone <- err
		}()
		<-disableStarted
		select {
		case err := <-disableDone:
			t.Errorf("disable completed before PATCH authorization lock released: %v", err)
			disableDone <- err
		case <-time.After(100 * time.Millisecond):
		}
	}
	api, closeRemote := remote.client(principal)
	defer closeRemote()
	if _, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-disableDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disable did not complete after PATCH authorization lock released")
	}
	remote.assertCounts(t, 2, 1, 1)
}

func TestLeadSetStatusWorkflowRejectsStaleLeaseAttempt(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 126, 50)
	command := LeadStatusCommand{LeadID: 5007, PipelineID: 6007, StatusID: 7007}
	stale := admitAndClaimLeadStatus(t, pool, principal, command)
	if _, err := pool.Exec(context.Background(), `
		UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := jobs.NewStore(pool).Claim(context.Background(), "replacement-worker", 1, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaimed jobs/error = %+v/%v", reclaimed, err)
	}
	remote := newLeadStatusRemote(command.LeadID, 1, 2)
	api, closeRemote := remote.client(principal)
	defer closeRemote()
	_, err = LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), stale)
	if failure := jobs.Classify(err, 1); err == nil || failure.Code != "action_not_authorized" {
		t.Fatalf("stale attempt error/failure = %v/%+v", err, failure)
	}
	remote.assertCounts(t, 0, 0, 0)
}

func TestLeadSetStatusWorkflowRejectsInactiveTenantAndNonAdmin(t *testing.T) {
	t.Run("inactive tenant makes no amoCRM request", func(t *testing.T) {
		pool := testkit.Postgres(t)
		testkit.Reset(t, pool)
		principal := widgetPrincipal(t, pool, 123, 47)
		command := LeadStatusCommand{LeadID: 5004, PipelineID: 6004, StatusID: 7004}
		job := admitAndClaimLeadStatus(t, pool, principal, command)
		if _, err := pool.Exec(context.Background(), `
			UPDATE integrations SET status='disabled' WHERE id=$1`, principal.IntegrationID); err != nil {
			t.Fatal(err)
		}
		remote := newLeadStatusRemote(command.LeadID, 1, 2)
		api, closeRemote := remote.client(principal)
		defer closeRemote()
		_, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job)
		if failure := jobs.Classify(err, 1); err == nil || failure.Code != "action_not_authorized" {
			t.Fatalf("inactive tenant error = %v", err)
		}
		remote.assertCounts(t, 0, 0, 0)
	})

	t.Run("non-admin cannot read or mutate lead", func(t *testing.T) {
		pool := testkit.Postgres(t)
		testkit.Reset(t, pool)
		principal := widgetPrincipal(t, pool, 124, 48)
		command := LeadStatusCommand{LeadID: 5005, PipelineID: 6005, StatusID: 7005}
		job := admitAndClaimLeadStatus(t, pool, principal, command)
		remote := newLeadStatusRemote(command.LeadID, 1, 2)
		remote.admin = false
		api, closeRemote := remote.client(principal)
		defer closeRemote()
		_, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job)
		if failure := jobs.Classify(err, 1); err == nil || failure.Code != "actor_forbidden" || failure.Retryable {
			t.Fatalf("non-admin error/failure = %v/%+v", err, failure)
		}
		remote.assertCounts(t, 1, 0, 0)
	})
}

func admitAndClaimLeadStatus(
	t *testing.T,
	pool *pgxpool.Pool,
	principal widgetauth.Principal,
	command LeadStatusCommand,
) jobs.Job {
	t.Helper()
	jobStore := jobs.NewStore(pool)
	if _, err := NewActionStore(pool, jobStore).EnqueueLeadSetStatus(
		context.Background(), principal, uuid.NewString(), command,
	); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.Claim(context.Background(), "workflow-test", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Type != LeadSetStatusJobType {
		t.Fatalf("claimed jobs = %+v", claimed)
	}
	return claimed[0]
}

type staticWorkflowTokenProvider struct {
	principal widgetauth.Principal
}

func (p staticWorkflowTokenProvider) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	return amocrm.AccessToken{
		InstallationID: p.principal.InstallationID,
		IntegrationID:  p.principal.IntegrationID,
		AccountID:      p.principal.AccountID,
		AccountDomain:  "tenant.amocrm.ru",
		Value:          "integration-access-token",
		TokenVersion:   1,
	}, nil
}

func (p staticWorkflowTokenProvider) RefreshIfCurrent(_ context.Context, token amocrm.AccessToken) (amocrm.AccessToken, error) {
	return token, nil
}

func (p staticWorkflowTokenProvider) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	return nil
}

type leadStatusRemote struct {
	mu                  sync.Mutex
	leadID              int64
	pipelineID          int64
	statusID            int64
	admin               bool
	failAfterFirstPatch bool
	failedPatch         bool
	afterLeadRead       func()
	beforePatchResponse func()
	userGets            int
	leadGets            int
	patches             int
}

func newLeadStatusRemote(leadID, pipelineID, statusID int64) *leadStatusRemote {
	return &leadStatusRemote{leadID: leadID, pipelineID: pipelineID, statusID: statusID, admin: true}
}

func (remote *leadStatusRemote) client(principal widgetauth.Principal) (*amocrm.Client, func()) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer integration-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		remote.mu.Lock()
		defer remote.mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v4/users/"+itoa(principal.UserID):
			remote.userGets++
			writeStubJSON(w, map[string]any{
				"id":     principal.UserID,
				"rights": map[string]bool{"is_admin": remote.admin, "is_active": true},
			})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v4/leads/"+itoa(remote.leadID):
			remote.leadGets++
			writeStubJSON(w, amocrm.LeadState{ID: remote.leadID, PipelineID: remote.pipelineID, StatusID: remote.statusID})
			if remote.afterLeadRead != nil {
				remote.afterLeadRead()
			}
		case request.Method == http.MethodPatch && request.URL.Path == "/api/v4/leads/"+itoa(remote.leadID):
			remote.patches++
			body, _ := io.ReadAll(request.Body)
			var update map[string]int64
			if err := json.NewDecoder(bytes.NewReader(body)).Decode(&update); err != nil {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			remote.pipelineID = update["pipeline_id"]
			remote.statusID = update["status_id"]
			if remote.beforePatchResponse != nil {
				remote.beforePatchResponse()
			}
			if remote.failAfterFirstPatch && !remote.failedPatch {
				remote.failedPatch = true
				http.Error(w, "ambiguous", http.StatusInternalServerError)
				return
			}
			writeStubJSON(w, map[string]any{"id": remote.leadID})
		default:
			http.NotFound(w, request)
		}
	}))
	target, _ := url.Parse(server.URL)
	client := server.Client()
	client.Transport = rewriteWorkflowTransport{base: client.Transport, target: target}
	return amocrm.NewClient(client, staticWorkflowTokenProvider{principal: principal}), server.Close
}

func (remote *leadStatusRemote) assertCounts(t *testing.T, users, leads, patches int) {
	t.Helper()
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.userGets != users || remote.leadGets != leads || remote.patches != patches {
		t.Fatalf("amoCRM calls user/lead/patch = %d/%d/%d, want %d/%d/%d",
			remote.userGets, remote.leadGets, remote.patches, users, leads, patches)
	}
}

type rewriteWorkflowTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (transport rewriteWorkflowTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clonedURL := *request.URL
	clonedURL.Scheme = transport.target.Scheme
	clonedURL.Host = transport.target.Host
	clone.URL = &clonedURL
	return transport.base.RoundTrip(clone)
}

func writeStubJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
