package webhook

import (
	"context"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestLeadStatusWebhookCapabilityControlsRoutingPerIntegration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	a := workflowInstallation(t, pool)
	b := workflowInstallation(t, pool) // same account, distinct integration
	if _, err := pool.Exec(ctx, `UPDATE integration_services SET enabled=false
		WHERE integration_id=(SELECT integration_id FROM installations WHERE id=$1)`, b); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lead_status_workflow_rules
		(installation_id,source_pipeline_id,source_status_id,target_pipeline_id,target_status_id)
		VALUES ($1,10,20,10,30),($2,10,20,10,30)`, a, b); err != nil {
		t.Fatal(err)
	}
	store := newWorkflowTestStore(pool)
	raw := []byte("account[id]=42&leads[status][0][id]=301&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20")
	aEvent := saveAndParseWorkflowEvent(t, store, a, raw)
	bEvent := saveAndParseWorkflowEvent(t, store, b, raw)
	if err := store.ProcessEvent(ctx, aEvent, a); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessEvent(ctx, bEvent, b); err != nil {
		t.Fatal(err)
	}
	var aJobs, bJobs, bRuns int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM jobs WHERE installation_id=$1 AND type=$3),
		(SELECT count(*) FROM jobs WHERE installation_id=$2 AND type=$3),
		(SELECT count(*) FROM workflow_runs WHERE installation_id=$2)`,
		a, b, leadstatus.LeadStatusTransitionJobType).Scan(&aJobs, &bJobs, &bRuns); err != nil {
		t.Fatal(err)
	}
	if aJobs != 1 || bJobs != 0 || bRuns != 0 {
		t.Fatalf("routed A jobs / B jobs / B runs = %d / %d / %d", aJobs, bJobs, bRuns)
	}
}

func TestLeadStatusWebhookCapabilityRecheckedBeforeSideEffect(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		name := "before_worker"
		if duringRead {
			name = "before_patch"
		}
		t.Run(name, func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			ctx := context.Background()
			installationID, store, job := claimLeadStatusWorkflow(t, pool, 302)
			revoke := func() {
				if _, err := pool.Exec(ctx, `UPDATE integration_services SET enabled=false
					WHERE integration_id=(SELECT integration_id FROM installations WHERE id=$1)`, installationID); err != nil {
					t.Errorf("revoke capability: %v", err)
				}
			}
			remote := newTransitionRemote(302, 10, 20)
			if duringRead {
				remote.afterLeadRead = revoke
			} else {
				revoke()
			}
			api, closeRemote := remote.client(t, pool, installationID)
			defer closeRemote()
			_, err := testTransitionHandler(store, leadstatus.NewExecutionStore(pool), api)(ctx, job)
			failure := jobs.Classify(err, 1)
			if err == nil || failure.Code != "action_not_authorized" || failure.Retryable {
				t.Fatalf("revoked webhook workflow = %v, %+v", err, failure)
			}
			_, _, gets, patches := remote.snapshot()
			wantGets := 0
			if duringRead {
				wantGets = 1
			}
			if gets != wantGets || patches != 0 {
				t.Fatalf("gets / patches = %d / %d", gets, patches)
			}
		})
	}
}
