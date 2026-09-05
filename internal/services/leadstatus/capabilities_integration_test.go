package leadstatus

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func TestLeadStatusCapabilityAdmissionIsolatedForSameAccount(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	a := widgetPrincipal(t, pool, 301, 71)
	b := widgetPrincipal(t, pool, 301, 71)
	b.TokenID = a.TokenID
	if _, err := pool.Exec(ctx, `DELETE FROM integration_services WHERE integration_id=$1`, b.IntegrationID); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(pool)
	actions := NewActionStore(pool, jobStore)
	handler := newTestHandler(jobStore, actions)
	assertCounts := func(want int) {
		t.Helper()
		var tokens, keys, countJobs int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM used_widget_tokens),
			(SELECT count(*) FROM idempotency_keys),
			(SELECT count(*) FROM jobs)`).Scan(&tokens, &keys, &countJobs); err != nil {
			t.Fatal(err)
		}
		if tokens != want || keys != want || countJobs != want {
			t.Fatalf("tokens/keys/jobs = %d/%d/%d, want each %d", tokens, keys, countJobs, want)
		}
	}
	for _, route := range []struct {
		body   string
		handle http.HandlerFunc
	}{
		{`{"lead_id":501,"pipeline_id":601,"status_id":701}`, handler.LeadSetStatus},
		{`{"source_pipeline_id":10,"source_status_id":20,"target_pipeline_id":10,"target_status_id":30,"enabled":true,"expected_revision":0}`, handler.ConfigureLeadStatusRule},
	} {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(route.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "shared-key")
		request = request.WithContext(widgetauth.ContextWithPrincipal(ctx, b))
		response := httptest.NewRecorder()
		route.handle(response, request)
		if response.Code != http.StatusForbidden || response.Body.String() != "{\"error\":{\"code\":\"service_not_enabled\"}}\n" {
			t.Fatalf("denied response = %d %s", response.Code, response.Body)
		}
	}
	assertCounts(0)
	command := LeadStatusCommand{LeadID: 501, PipelineID: 601, StatusID: 701}
	first, err := actions.EnqueueLeadSetStatus(ctx, a, "shared-key", command)
	if err != nil {
		t.Fatal(err)
	}
	// The same JWT id and idempotency key remain usable after granting B.
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services (integration_id,service_code,enabled) VALUES ($1,'lead-status',true)`, b.IntegrationID); err != nil {
		t.Fatal(err)
	}
	second, err := actions.EnqueueLeadSetStatus(ctx, b, "shared-key", command)
	if err != nil || first.JobID == second.JobID {
		t.Fatalf("independent B admission = %+v, %v", second, err)
	}
	assertCounts(2)
	if _, err := jobStore.GetForInstallationActor(ctx, second.JobID, a.InstallationID, widgetActorType, strconv.FormatInt(a.UserID, 10)); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("A reading B job in the same account = %v", err)
	}
	// The checker binds both identities; swapping integrations never authorizes.
	enabled, err := services.NewStore(pool).IsEnabled(ctx, a.IntegrationID, b.InstallationID, services.LeadStatus)
	if err != nil || enabled {
		t.Fatalf("mixed tenant capability = %t, %v", enabled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1`, a.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := actions.EnqueueLeadSetStatus(ctx, a, "shared-key", command); !errors.Is(err, services.ErrNotEnabled) {
		t.Fatalf("revoked replay should deny before JWT replay: %v", err)
	}
	enabled, err = services.NewStore(pool).IsEnabled(ctx, b.IntegrationID, b.InstallationID, services.LeadStatus)
	if err != nil || !enabled {
		t.Fatalf("A revocation affected B: %t, %v", enabled, err)
	}
}

func TestLeadStatusCapabilityRevocationPreventsWorkerSideEffect(t *testing.T) {
	for _, revokeDuringRead := range []bool{false, true} {
		name := "before_worker"
		if revokeDuringRead {
			name = "before_patch"
		}
		t.Run(name, func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			principal := widgetPrincipal(t, pool, 302, 72)
			command := LeadStatusCommand{LeadID: 5001, PipelineID: 6001, StatusID: 7001}
			job := admitAndClaimLeadStatus(t, pool, principal, command)
			revoke := func() {
				if _, err := pool.Exec(context.Background(), `UPDATE integration_services SET enabled=false WHERE integration_id=$1`, principal.IntegrationID); err != nil {
					t.Errorf("revoke capability: %v", err)
				}
			}
			remote := newLeadStatusRemote(command.LeadID, 1, 2)
			if revokeDuringRead {
				remote.afterLeadRead = revoke
			} else {
				revoke()
			}
			api, closeRemote := remote.client(principal)
			defer closeRemote()
			_, err := LeadSetStatusJobHandler(NewExecutionStore(pool), api)(context.Background(), job)
			failure := jobs.Classify(err, 1)
			if err == nil || failure.Code != "action_not_authorized" || failure.Retryable {
				t.Fatalf("revoked worker = %v, %+v", err, failure)
			}
			if revokeDuringRead {
				remote.assertCounts(t, 2, 1, 0)
			} else {
				remote.assertCounts(t, 0, 0, 0)
			}
		})
	}
}

func TestLeadStatusRuleRevocationBeforeConfiguration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	principal := widgetPrincipal(t, pool, 303, 73)
	command := LeadStatusRuleCommand{SourcePipelineID: 10, SourceStatusID: 20, TargetPipelineID: 10, TargetStatusID: 30, Enabled: true}
	job := admitAndClaimRule(t, pool, principal, command, "rule-revoked")
	if _, err := pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1`, principal.IntegrationID); err != nil {
		t.Fatal(err)
	}
	// Configure itself rechecks under a lock, after any remote administrator read.
	_, err := NewRuleStore(pool).Configure(ctx, job, principal.UserID, command)
	if !errors.Is(err, ErrExecutionNotAuthorized) {
		t.Fatalf("revoked rule configuration = %v", err)
	}
	assertRuleCount(t, pool, 0)
}
