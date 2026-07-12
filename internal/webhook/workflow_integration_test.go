package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	remote := newTransitionRemote(100, 10, 20)
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()
	handler := LeadStatusTransitionJobHandler(store, widgetapi.NewExecutionStore(pool), api)
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
	pipelineID, statusID, _, patches := remote.snapshot()
	if patches != 1 || pipelineID != 10 || statusID != 30 ||
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

func TestLeadStatusWorkflowDoesNotOverwriteChangedSourceState(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID, store, workflowJob := claimLeadStatusWorkflow(t, pool, 200)

	var payload leadStatusTransitionPayload
	if err := json.Unmarshal(workflowJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SourcePipelineID != 10 || payload.SourceStatusID != 20 ||
		payload.PipelineID != 10 || payload.StatusID != 30 {
		t.Fatalf("workflow payload does not preserve source/target: %+v", payload)
	}

	remote := newTransitionRemote(200, 10, 40)
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()
	result, err := LeadStatusTransitionJobHandler(
		store, widgetapi.NewExecutionStore(pool), api,
	)(ctx, workflowJob)
	if err != nil {
		t.Fatal(err)
	}
	_, _, gets, patches := remote.snapshot()
	if gets != 1 || patches != 0 || string(result) !=
		`{"converged":false,"lead_id":200,"pipeline_id":10,"status_id":30}` {
		t.Fatalf("changed source remote/result = %+v/%s", remote, result)
	}
	if err := jobs.NewStore(pool).Complete(
		ctx, workflowJob, "source-guard-test", result, time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}

	var runStatus, jobStatus, auditOutcome string
	var effects, runs, workflowJobs int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT status FROM workflow_runs WHERE job_id=$1),
			(SELECT status FROM jobs WHERE id=$1),
			(SELECT metadata->>'outcome' FROM audit_log WHERE correlation_job_id=$1),
			(SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1),
			(SELECT count(*) FROM workflow_runs WHERE installation_id=$2),
			(SELECT count(*) FROM jobs WHERE installation_id=$2 AND type=$3)`,
		workflowJob.ID, installationID, LeadStatusTransitionJobType,
	).Scan(&runStatus, &jobStatus, &auditOutcome, &effects, &runs, &workflowJobs); err != nil {
		t.Fatal(err)
	}
	if runStatus != "completed" || jobStatus != "completed" || auditOutcome != "source_changed" ||
		effects != 0 || runs != 1 || workflowJobs != 1 {
		t.Fatalf("run/job/audit/effects/counts=%s/%s/%s/%d/%d/%d",
			runStatus, jobStatus, auditOutcome, effects, runs, workflowJobs)
	}
}

func TestLeadStatusWorkflowSourceChangedRechecksLeaseBeforeCompletion(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID, store, workflowJob := claimLeadStatusWorkflow(t, pool, 204)
	remote := newTransitionRemote(204, 10, 40)
	remote.afterLeadRead = func() {
		if _, err := pool.Exec(ctx, `
			UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`,
			workflowJob.ID,
		); err != nil {
			t.Errorf("expire workflow lease: %v", err)
		}
	}
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()

	_, err := LeadStatusTransitionJobHandler(
		store, widgetapi.NewExecutionStore(pool), api,
	)(ctx, workflowJob)
	if !errors.Is(err, widgetapi.ErrExecutionNotAuthorized) {
		t.Fatalf("stale completion error = %v", err)
	}
	var runStatus, jobStatus string
	var audits, effects int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT status FROM workflow_runs WHERE job_id=$1),
		(SELECT status FROM jobs WHERE id=$1),
		(SELECT count(*) FROM audit_log WHERE correlation_job_id=$1),
		(SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1)`,
		workflowJob.ID,
	).Scan(&runStatus, &jobStatus, &audits, &effects); err != nil {
		t.Fatal(err)
	}
	_, _, gets, patches := remote.snapshot()
	if gets != 1 || patches != 0 || runStatus != "processing" || jobStatus != "processing" ||
		audits != 0 || effects != 0 {
		t.Fatalf("stale completion gets/patches/run/job/audits/effects=%d/%d/%s/%s/%d/%d",
			gets, patches, runStatus, jobStatus, audits, effects)
	}
}

func TestLeadStatusWorkflowCompletedReceiptSurvivesJobCompletionCrash(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID, store, workflowJob := claimLeadStatusWorkflow(t, pool, 205)
	remote := newTransitionRemote(205, 10, 40)
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()
	handler := LeadStatusTransitionJobHandler(store, widgetapi.NewExecutionStore(pool), api)

	firstResult, err := handler(ctx, workflowJob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`,
		workflowJob.ID,
	); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(pool)
	reclaimed, err := jobStore.ClaimWithObserver(
		ctx, "source-receipt-retry", 1, 100, time.Minute, JobFailureObserver(store),
	)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != workflowJob.ID || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaim completed receipt job/error = %+v/%v", reclaimed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='disabled' WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	secondResult, err := handler(ctx, reclaimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Complete(
		ctx, reclaimed[0], "source-receipt-retry", secondResult, time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}

	var runStatus, jobStatus string
	var attempts, audits, effects int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT status FROM workflow_runs WHERE job_id=$1),
		(SELECT status FROM jobs WHERE id=$1),
		(SELECT attempts FROM jobs WHERE id=$1),
		(SELECT count(*) FROM audit_log WHERE correlation_job_id=$1),
		(SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1)`,
		workflowJob.ID,
	).Scan(&runStatus, &jobStatus, &attempts, &audits, &effects); err != nil {
		t.Fatal(err)
	}
	_, _, gets, patches := remote.snapshot()
	if string(firstResult) != string(secondResult) ||
		string(secondResult) != `{"converged":false,"lead_id":205,"pipeline_id":10,"status_id":30}` ||
		gets != 1 || patches != 0 || runStatus != "completed" || jobStatus != "completed" ||
		attempts != 2 || audits != 1 || effects != 0 {
		t.Fatalf("receipt results/remote/run/job/attempts/audits/effects=%s/%s/%d/%d/%s/%s/%d/%d/%d",
			firstResult, secondResult, gets, patches, runStatus, jobStatus, attempts, audits, effects)
	}
}

func TestLeadStatusWorkflowAlreadyAtTargetDoesNotCreateEffect(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID, store, workflowJob := claimLeadStatusWorkflow(t, pool, 201)
	remote := newTransitionRemote(201, 10, 30)
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()

	result, err := LeadStatusTransitionJobHandler(
		store, widgetapi.NewExecutionStore(pool), api,
	)(ctx, workflowJob)
	if err != nil {
		t.Fatal(err)
	}
	var effects int
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1),
		(SELECT metadata->>'outcome' FROM audit_log WHERE correlation_job_id=$1)`,
		workflowJob.ID,
	).Scan(&effects, &outcome); err != nil {
		t.Fatal(err)
	}
	if err := jobs.NewStore(pool).Complete(
		ctx, workflowJob, "source-guard-test", result, time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	var jobStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, workflowJob.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	_, _, gets, patches := remote.snapshot()
	if gets != 1 || patches != 0 || effects != 0 || outcome != "already_converged" || jobStatus != "completed" ||
		string(result) != `{"converged":true,"lead_id":201,"pipeline_id":10,"status_id":30}` {
		t.Fatalf("target remote/effects/outcome/job/result=%+v/%d/%s/%s/%s",
			remote, effects, outcome, jobStatus, result)
	}
}

func TestLeadStatusWorkflowUncertainAppliedEffectIsNotPatchedAgain(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	installationID, store, workflowJob := claimLeadStatusWorkflow(t, pool, 202)
	remote := newTransitionRemote(202, 10, 20)
	remote.failAfterFirstPatch = true
	api, closeRemote := remote.client(t, pool, installationID)
	defer closeRemote()
	handler := LeadStatusTransitionJobHandler(store, widgetapi.NewExecutionStore(pool), api)

	_, firstErr := handler(ctx, workflowJob)
	if firstErr == nil || !jobs.Classify(firstErr, workflowJob.Attempts).Retryable {
		t.Fatalf("uncertain remote effect must remain retryable, got %v", firstErr)
	}
	pipelineID, statusID, _, patches := remote.snapshot()
	if patches != 1 || pipelineID != 10 || statusID != 30 {
		t.Fatalf("first uncertain mutation = %+v", remote)
	}
	jobStore := jobs.NewStore(pool)
	workerID := *workflowJob.LockedBy
	if status, err := jobStore.FailWithObserver(
		ctx, workflowJob, workerID, jobs.Classify(firstErr, workflowJob.Attempts),
		time.Millisecond, JobFailureObserver(store),
	); err != nil || status != jobs.StatusRetry {
		t.Fatalf("fail uncertain workflow status/error = %s/%v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET run_after=now() WHERE id=$1`, workflowJob.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := jobStore.Claim(ctx, "source-guard-retry", 1, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != workflowJob.ID || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaimed workflow jobs/error = %+v/%v", reclaimed, err)
	}
	result, err := handler(ctx, reclaimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Complete(
		ctx, reclaimed[0], "source-guard-retry", result, time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	var effectState string
	var runStatus, jobStatus string
	var effectCount int
	if err := pool.QueryRow(ctx, `SELECT state FROM outbound_effects WHERE correlation_job_id=$1`, workflowJob.ID).
		Scan(&effectState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT status FROM workflow_runs WHERE job_id=$1),
		(SELECT status FROM jobs WHERE id=$1),
		(SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1)`,
		workflowJob.ID,
	).Scan(&runStatus, &jobStatus, &effectCount); err != nil {
		t.Fatal(err)
	}
	_, _, gets, patches := remote.snapshot()
	if gets != 2 || patches != 1 || effectState != "applied" ||
		runStatus != "completed" || jobStatus != "completed" || effectCount != 1 ||
		string(result) != `{"converged":true,"lead_id":202,"pipeline_id":10,"status_id":30}` {
		t.Fatalf("retry remote/effect/run/job/count/result=%+v/%s/%s/%s/%d/%s",
			remote, effectState, runStatus, jobStatus, effectCount, result)
	}
}

func TestLeadStatusWorkflowRejectsIncompleteOrMalformedState(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing source", mutate: func(payload map[string]any) {
			delete(payload, "source_pipeline_id")
		}},
		{name: "malformed source", mutate: func(payload map[string]any) {
			payload["source_status_id"] = "invalid"
		}},
		{name: "missing target", mutate: func(payload map[string]any) {
			delete(payload, "pipeline_id")
		}},
		{name: "zero target", mutate: func(payload map[string]any) {
			payload["status_id"] = 0
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			ctx := context.Background()
			installationID := workflowInstallation(t, pool)
			payload := map[string]any{
				"workflow_run_id": uuid.New(), "lead_id": 203,
				"source_pipeline_id": 10, "source_status_id": 20,
				"pipeline_id": 10, "status_id": 30,
			}
			testCase.mutate(payload)
			job, err := jobs.NewStore(pool).Enqueue(ctx, jobs.EnqueueParams{
				InstallationID: &installationID, Type: LeadStatusTransitionJobType,
				ActorType: "integration", ActorID: installationID.String(),
				ResourceType: leadResourceType, ResourceID: "203", Payload: payload,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := jobs.NewStore(pool).Claim(ctx, "source-guard-test", 1, time.Minute)
			if err != nil || len(claimed) != 1 || claimed[0].ID != job.ID {
				t.Fatalf("claim malformed workflow: jobs=%#v err=%v", claimed, err)
			}
			remote := newTransitionRemote(203, 10, 20)
			api, closeRemote := remote.client(t, pool, installationID)
			defer closeRemote()
			_, handlerErr := LeadStatusTransitionJobHandler(
				NewStore(pool), widgetapi.NewExecutionStore(pool), api,
			)(ctx, claimed[0])
			if handlerErr == nil {
				t.Fatal("incomplete or malformed workflow payload was accepted")
			}
			failure := jobs.Classify(handlerErr, claimed[0].Attempts)
			var effects int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM outbound_effects WHERE correlation_job_id=$1`, job.ID,
			).Scan(&effects); err != nil {
				t.Fatal(err)
			}
			_, _, gets, patches := remote.snapshot()
			if failure.Retryable || failure.Code != "invalid_payload" ||
				gets != 0 || patches != 0 || effects != 0 {
				t.Fatalf("malformed payload failure/remote/effects=%+v/%+v/%d",
					failure, remote, effects)
			}
		})
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

func claimLeadStatusWorkflow(
	t *testing.T,
	pool *pgxpool.Pool,
	leadID int64,
) (uuid.UUID, *Store, jobs.Job) {
	t.Helper()
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
	raw := []byte(fmt.Sprintf(
		"account[id]=42&leads[status][0][id]=%d&leads[status][0][pipeline_id]=10&leads[status][0][status_id]=20",
		leadID,
	))
	eventID := saveAndParseWorkflowEvent(t, store, installationID, raw)
	if err := store.ProcessEvent(ctx, eventID, installationID); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.NewStore(pool).Claim(ctx, "source-guard-test", 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range claimed {
		if job.Type == LeadStatusTransitionJobType {
			return installationID, store, job
		}
	}
	t.Fatalf("workflow job not claimed: %#v", claimed)
	return uuid.Nil, nil, jobs.Job{}
}

type transitionRemote struct {
	mu                  sync.Mutex
	leadID              int64
	pipelineID          int64
	statusID            int64
	leadGets            int
	patches             int
	failAfterFirstPatch bool
	failedPatch         bool
	afterLeadRead       func()
}

func newTransitionRemote(leadID, pipelineID, statusID int64) *transitionRemote {
	return &transitionRemote{leadID: leadID, pipelineID: pipelineID, statusID: statusID}
}

func (remote *transitionRemote) client(
	t *testing.T,
	pool *pgxpool.Pool,
	installationID uuid.UUID,
) (*amocrm.Client, func()) {
	t.Helper()
	var integrationID uuid.UUID
	var accountID int64
	var accountDomain string
	if err := pool.QueryRow(context.Background(), `
		SELECT integration_id,account_id,account_domain
		FROM installations WHERE id=$1`, installationID,
	).Scan(&integrationID, &accountID, &accountDomain); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer integration-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v4/leads/"+strconv.FormatInt(remote.leadID, 10):
			remote.mu.Lock()
			remote.leadGets++
			state := amocrm.LeadState{
				ID: remote.leadID, PipelineID: remote.pipelineID, StatusID: remote.statusID,
			}
			afterRead := remote.afterLeadRead
			remote.mu.Unlock()
			if afterRead != nil {
				afterRead()
			}
			writeTransitionJSON(w, state)
		case request.Method == http.MethodPatch && request.URL.Path == "/api/v4/leads/"+strconv.FormatInt(remote.leadID, 10):
			body, _ := io.ReadAll(request.Body)
			var update map[string]int64
			if err := json.NewDecoder(bytes.NewReader(body)).Decode(&update); err != nil {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			remote.mu.Lock()
			remote.patches++
			remote.pipelineID = update["pipeline_id"]
			remote.statusID = update["status_id"]
			fail := remote.failAfterFirstPatch && !remote.failedPatch
			if fail {
				remote.failedPatch = true
			}
			remote.mu.Unlock()
			if fail {
				http.Error(w, "ambiguous", http.StatusInternalServerError)
				return
			}
			writeTransitionJSON(w, map[string]any{"id": remote.leadID})
		default:
			http.NotFound(w, request)
		}
	}))
	target, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client := server.Client()
	client.Transport = rewriteTransitionTransport{base: client.Transport, target: target}
	tokens := staticTransitionTokenProvider{token: amocrm.AccessToken{
		InstallationID: installationID, IntegrationID: integrationID,
		AccountID: accountID, AccountDomain: accountDomain,
		Value: "integration-access-token", TokenVersion: 1,
	}}
	return amocrm.NewClient(client, tokens), server.Close
}

func (remote *transitionRemote) snapshot() (pipelineID, statusID int64, gets, patches int) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	return remote.pipelineID, remote.statusID, remote.leadGets, remote.patches
}

type staticTransitionTokenProvider struct {
	token amocrm.AccessToken
}

func (provider staticTransitionTokenProvider) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	return provider.token, nil
}

func (provider staticTransitionTokenProvider) RefreshIfCurrent(
	_ context.Context,
	token amocrm.AccessToken,
) (amocrm.AccessToken, error) {
	return token, nil
}

func (staticTransitionTokenProvider) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	return nil
}

type rewriteTransitionTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (transport rewriteTransitionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clonedURL := *request.URL
	clonedURL.Scheme = transport.target.Scheme
	clonedURL.Host = transport.target.Host
	clone.URL = &clonedURL
	return transport.base.RoundTrip(clone)
}

func writeTransitionJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
