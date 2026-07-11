package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

func TestLeadStatusWebhookDispatchIsDurablyDeduplicated(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID := workflowInstallation(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO lead_status_workflow_rules (
			installation_id, source_pipeline_id, source_status_id,
			target_pipeline_id, target_status_id
		) VALUES ($1, 10, 20, 10, 30)`, installationID); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	raw := []byte("account[id]=42&leads[status][0][id]=100&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20")
	deliveryID, err := store.SaveDeliveryAndEnqueue(ctx, installationID, uuid.New(), "application/x-www-form-urlencoded", raw)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := store.GetDelivery(ctx, deliveryID, installationID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ParseAllowed(installationID, raw, []string{"status_lead"})
	if err != nil || len(events) != 1 {
		t.Fatalf("parse events=%d err=%v", len(events), err)
	}
	if inserted, err := store.SaveParsedEvents(ctx, delivery, events); err != nil || inserted != 1 {
		t.Fatalf("inserted=%d err=%v", inserted, err)
	}
	var eventID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM inbox_events WHERE installation_id=$1`, installationID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessEvent(ctx, eventID, installationID); err != nil {
		t.Fatal(err)
	}

	secondID, err := store.SaveDeliveryAndEnqueue(ctx, installationID, uuid.New(), "application/x-www-form-urlencoded", raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.GetDelivery(ctx, secondID, installationID)
	if err != nil {
		t.Fatal(err)
	}
	if inserted, err := store.SaveParsedEvents(ctx, second, events); err != nil || inserted != 0 {
		t.Fatalf("duplicate inserted=%d err=%v", inserted, err)
	}
	var tombstones, eventCount, runCount, workflowJobs int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM webhook_event_tombstones WHERE installation_id=$1),
			(SELECT count(*) FROM inbox_events WHERE installation_id=$1),
			(SELECT count(*) FROM workflow_runs WHERE installation_id=$1),
			(SELECT count(*) FROM jobs WHERE installation_id=$1 AND type=$2)`,
		installationID, LeadStatusTransitionJobType,
	).Scan(&tombstones, &eventCount, &runCount, &workflowJobs); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || eventCount != 1 || runCount != 1 || workflowJobs != 1 {
		t.Fatalf("tombstones/events/runs/jobs=%d/%d/%d/%d", tombstones, eventCount, runCount, workflowJobs)
	}
}

func TestLeadStatusWebhookRuleEnabledFlagControlsRouting(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID := workflowInstallation(t, pool)
	var ruleID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO lead_status_workflow_rules (
			installation_id,source_pipeline_id,source_status_id,
			target_pipeline_id,target_status_id,enabled
		) VALUES ($1,10,20,10,30,false) RETURNING id`, installationID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	disabledEvent := saveAndParseWorkflowEvent(t, store, installationID,
		[]byte("account[id]=42&leads[status][0][id]=101&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20"))
	if err := store.ProcessEvent(ctx, disabledEvent, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE lead_status_workflow_rules SET enabled=true WHERE id=$1`, ruleID); err != nil {
		t.Fatal(err)
	}
	enabledEvent := saveAndParseWorkflowEvent(t, store, installationID,
		[]byte("account[id]=42&leads[status][0][id]=102&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20"))
	if err := store.ProcessEvent(ctx, enabledEvent, installationID); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE rule_id=$1`, ruleID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("workflow runs after disabled/enabled events = %d", runs)
	}
}

func TestLeadStatusWebhookWorkflowConvergesAndCorrelatesItsEffect(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID := workflowInstallation(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO lead_status_workflow_rules (
			installation_id, source_pipeline_id, source_status_id,
			target_pipeline_id, target_status_id
		) VALUES ($1, 10, 20, 10, 30)`, installationID); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	sourceRaw := []byte("account[id]=42&leads[status][0][id]=100&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20")
	sourceEventID := saveAndParseWorkflowEvent(t, store, installationID, sourceRaw)
	if err := store.ProcessEvent(ctx, sourceEventID, installationID); err != nil {
		t.Fatal(err)
	}

	claimed, err := jobs.NewStore(pool).Claim(ctx, "workflow-test", 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var workflowJob jobs.Job
	for _, job := range claimed {
		if job.Type == LeadStatusTransitionJobType {
			workflowJob = job
			break
		}
	}
	if workflowJob.ID == uuid.Nil {
		t.Fatalf("workflow job not claimed: %#v", claimed)
	}
	remote := &transitionAPI{state: amocrm.LeadState{ID: 100, PipelineID: 10, StatusID: 20}}
	handler := LeadStatusTransitionJobHandler(store, widgetapi.NewExecutionStore(pool), remote)
	cancelledContext, cancel := context.WithCancel(ctx)
	cancel()
	if _, gateErr := handler(cancelledContext, workflowJob); gateErr == nil ||
		!jobs.Classify(gateErr, workflowJob.Attempts).Retryable {
		t.Fatalf("cancelled database gate must remain retryable, got %v", gateErr)
	}
	result, err := handler(ctx, workflowJob)
	if err != nil {
		t.Fatal(err)
	}
	if remote.patches != 1 || remote.state.PipelineID != 10 || remote.state.StatusID != 30 ||
		string(result) != `{"converged":true,"lead_id":100,"pipeline_id":10,"status_id":30}` {
		t.Fatalf("remote/result = %+v/%s", remote, result)
	}
	var runStatus, effectState string
	if err := pool.QueryRow(ctx, `
		SELECT run.status, effect.state
		FROM workflow_runs AS run
		JOIN outbound_effects AS effect ON effect.workflow_run_id=run.id
		WHERE run.job_id=$1`, workflowJob.ID,
	).Scan(&runStatus, &effectState); err != nil {
		t.Fatal(err)
	}
	if runStatus != "completed" || effectState != "applied" {
		t.Fatalf("run/effect = %s/%s", runStatus, effectState)
	}

	targetRaw := []byte("account[id]=42&leads[status][0][id]=100&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=30")
	targetEventID := saveAndParseWorkflowEvent(t, store, installationID, targetRaw)
	if err := store.ProcessEvent(ctx, targetEventID, installationID); err != nil {
		t.Fatal(err)
	}
	var targetStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM inbox_events WHERE id=$1`, targetEventID).Scan(&targetStatus); err != nil {
		t.Fatal(err)
	}
	if targetStatus != "ignored" {
		t.Fatalf("target event status = %s", targetStatus)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM outbound_effects WHERE correlation_job_id=$1`, workflowJob.ID).
		Scan(&effectState); err != nil || effectState != "observed" {
		t.Fatalf("observed effect = %q, err=%v", effectState, err)
	}
	var runCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE installation_id=$1`, installationID).
		Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("workflow run count = %d, err=%v", runCount, err)
	}
}

func TestWidgetLeadStatusEffectCorrelatesOnlyAfterAttempt(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID := workflowInstallation(t, pool)
	job, err := jobs.NewStore(pool).Enqueue(ctx, jobs.EnqueueParams{
		InstallationID: &installationID, Type: widgetapi.LeadSetStatusJobType,
		ActorType: "widget_user", ActorID: "7", ResourceType: "lead", ResourceID: "100",
		Payload: map[string]int64{"lead_id": 100, "pipeline_id": 10, "status_id": 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := widgetapi.NewExecutionStore(pool)
	effectID, err := execution.PrepareLeadStatusEffect(ctx, job, nil, 100, 10, 30)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	raw := []byte("account[id]=42&leads[status][0][id]=100&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=30")
	deliveryID, err := store.SaveDeliveryAndEnqueue(ctx, installationID, uuid.New(), "application/x-www-form-urlencoded", raw)
	if err != nil {
		t.Fatal(err)
	}
	delivery, _ := store.GetDelivery(ctx, deliveryID, installationID)
	events, _ := ParseAllowed(installationID, raw, []string{"status_lead"})
	if _, err := store.SaveParsedEvents(ctx, delivery, events); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM inbox_events WHERE installation_id=$1`, installationID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessEvent(ctx, eventID, installationID); err != nil {
		t.Fatal(err)
	}
	var eventStatus, effectState string
	var correlated uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT event.status, event.correlated_effect_id, effect.state
		FROM inbox_events AS event
		JOIN outbound_effects AS effect ON effect.id=event.correlated_effect_id
		WHERE event.id=$1`, eventID,
	).Scan(&eventStatus, &correlated, &effectState); err != nil {
		t.Fatal(err)
	}
	if eventStatus != "ignored" || effectState != "observed" || correlated != effectID {
		t.Fatalf("event/effect/correlation=%s/%s/%s", eventStatus, effectState, correlated)
	}
}

func workflowInstallation(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	integrationID := uuid.New()
	installationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri, webhook_events
		) VALUES ($1, $2, $3, decode('00','hex'), 'https://example.test/oauth',
			'["status_lead"]'::jsonb)`,
		integrationID, "workflow-"+integrationID.String(), uuid.NewString(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status,
			webhook_status, webhook_settings
		) VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active', 'active',
			'["status_lead"]'::jsonb)`, installationID, integrationID,
	); err != nil {
		t.Fatal(err)
	}
	return installationID
}

func saveAndParseWorkflowEvent(
	t *testing.T,
	store *Store,
	installationID uuid.UUID,
	raw []byte,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	deliveryID, err := store.SaveDeliveryAndEnqueue(
		ctx, installationID, uuid.New(), "application/x-www-form-urlencoded", raw,
	)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := store.GetDelivery(ctx, deliveryID, installationID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ParseAllowed(installationID, raw, []string{"status_lead"})
	if err != nil || len(events) != 1 {
		t.Fatalf("parse events=%d err=%v", len(events), err)
	}
	if inserted, err := store.SaveParsedEvents(ctx, delivery, events); err != nil || inserted != 1 {
		t.Fatalf("save parsed inserted=%d err=%v", inserted, err)
	}
	var eventID uuid.UUID
	if err := store.pool.QueryRow(ctx, `
		SELECT id FROM inbox_events WHERE delivery_id=$1`, deliveryID,
	).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

type transitionAPI struct {
	state   amocrm.LeadState
	patches int
	err     error
}

func (api *transitionAPI) GetLeadState(context.Context, uuid.UUID, int64) (amocrm.LeadState, error) {
	return api.state, nil
}

func (api *transitionAPI) PrepareLeadStatus(context.Context, uuid.UUID) (amocrm.LeadStatusMutation, error) {
	return transitionMutation{api: api}, nil
}

type transitionMutation struct{ api *transitionAPI }

func (mutation transitionMutation) SetLeadStatus(_ context.Context, leadID, pipelineID, statusID int64) error {
	mutation.api.patches++
	mutation.api.state = amocrm.LeadState{ID: leadID, PipelineID: pipelineID, StatusID: statusID}
	return mutation.api.err
}

var _ LeadStatusTransitionAPI = (*transitionAPI)(nil)
var _ amocrm.LeadStatusMutation = transitionMutation{}
