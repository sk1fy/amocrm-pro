package widgetapi

import (
	"context"
	"encoding/json"
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

func TestLeadStatusRuleConfigureCreateUpdateAndReceiptRetry(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 201, 61)
	api := &staticRuleManagementAPI{admin: true, active: true}
	handler := LeadStatusRuleConfigureJobHandler(NewExecutionStore(pool), NewRuleStore(pool), api)

	create := LeadStatusRuleCommand{
		SourcePipelineID: 10, SourceStatusID: 20,
		TargetPipelineID: 30, TargetStatusID: 40,
		Enabled: true, ExpectedRevision: 0,
	}
	job := admitAndClaimRule(t, pool, principal, create, "create-rule")
	encoded, err := handler(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	var created LeadStatusRuleResult
	if err := json.Unmarshal(encoded, &created); err != nil || created.RuleID == uuid.Nil || created.Revision != 1 {
		t.Fatalf("created result/error = %+v/%v", created, err)
	}
	if retry, err := handler(context.Background(), job); err != nil || string(retry) != string(encoded) {
		t.Fatalf("receipt retry result/error = %s/%v", retry, err)
	}
	if api.Calls() != 1 {
		t.Fatalf("admin calls after receipt retry = %d", api.Calls())
	}
	completeRuleJob(t, pool, job, encoded)

	fresh := principal
	fresh.TokenID = uuid.NewString()
	update := create
	update.TargetStatusID = 41
	update.Enabled = false
	update.ExpectedRevision = 1
	updatedJob := admitAndClaimRule(t, pool, fresh, update, "update-rule")
	encoded, err = handler(context.Background(), updatedJob)
	if err != nil {
		t.Fatal(err)
	}
	var updated LeadStatusRuleResult
	if err := json.Unmarshal(encoded, &updated); err != nil ||
		updated.RuleID != created.RuleID || updated.Revision != 2 || updated.Enabled {
		t.Fatalf("updated result/error = %+v/%v", updated, err)
	}
	var receipts, audits int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM lead_status_workflow_rule_configurations WHERE rule_id=$1),
		(SELECT count(*) FROM audit_log WHERE object_id=$2)`,
		created.RuleID, created.RuleID.String()).Scan(&receipts, &audits); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 || audits != 2 {
		t.Fatalf("receipts/audits = %d/%d", receipts, audits)
	}
}

func TestLeadStatusRuleConfigureCASAllowsOneWinner(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 202, 62)
	api := &staticRuleManagementAPI{admin: true, active: true}
	handler := LeadStatusRuleConfigureJobHandler(NewExecutionStore(pool), NewRuleStore(pool), api)
	base := LeadStatusRuleCommand{
		SourcePipelineID: 11, SourceStatusID: 21,
		TargetPipelineID: 31, TargetStatusID: 41, Enabled: true,
	}
	createJob := admitAndClaimRule(t, pool, principal, base, "cas-create")
	created, err := handler(context.Background(), createJob)
	if err != nil {
		t.Fatal(err)
	}
	completeRuleJob(t, pool, createJob, created)

	commands := []LeadStatusRuleCommand{base, base}
	commands[0].ExpectedRevision, commands[1].ExpectedRevision = 1, 1
	commands[0].TargetStatusID, commands[1].TargetStatusID = 42, 43
	jobsToRun := make([]jobs.Job, 0, 2)
	jobStore := jobs.NewStore(pool)
	for _, command := range commands {
		fresh := principal
		fresh.TokenID = uuid.NewString()
		if _, err := NewActionStore(pool, jobStore).EnqueueLeadStatusRuleConfigure(
			context.Background(), fresh, uuid.NewString(), command,
		); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := jobStore.Claim(context.Background(), "rule-cas", 2, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed jobs/error = %+v/%v", claimed, err)
	}
	jobsToRun = append(jobsToRun, claimed...)

	var wait sync.WaitGroup
	wait.Add(2)
	errorsSeen := make(chan error, 2)
	for _, job := range jobsToRun {
		go func(job jobs.Job) {
			defer wait.Done()
			_, err := handler(context.Background(), job)
			errorsSeen <- err
		}(job)
	}
	wait.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case jobs.Classify(err, 1).Code == "revision_conflict":
			conflicts++
		default:
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS success/conflict = %d/%d", successes, conflicts)
	}
	var revision int64
	if err := pool.QueryRow(context.Background(), `
		SELECT revision FROM lead_status_workflow_rules
		WHERE installation_id=$1 AND source_pipeline_id=11 AND source_status_id=21`,
		principal.InstallationID).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("final revision/error = %d/%v", revision, err)
	}
}

func TestLeadStatusRuleConfigureRejectsNonAdminAndStaleLease(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 203, 63)
	command := LeadStatusRuleCommand{
		SourcePipelineID: 12, SourceStatusID: 22,
		TargetPipelineID: 32, TargetStatusID: 42, Enabled: true,
	}
	job := admitAndClaimRule(t, pool, principal, command, "non-admin")
	nonAdmin := &staticRuleManagementAPI{active: true}
	_, err := LeadStatusRuleConfigureJobHandler(
		NewExecutionStore(pool), NewRuleStore(pool), nonAdmin,
	)(context.Background(), job)
	if failure := jobs.Classify(err, 1); err == nil || failure.Code != "actor_forbidden" {
		t.Fatalf("non-admin error/failure = %v/%+v", err, failure)
	}
	assertRuleCount(t, pool, 0)

	if _, err := pool.Exec(context.Background(), `
		UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	admin := &staticRuleManagementAPI{admin: true, active: true}
	_, err = LeadStatusRuleConfigureJobHandler(
		NewExecutionStore(pool), NewRuleStore(pool), admin,
	)(context.Background(), job)
	if failure := jobs.Classify(err, 1); err == nil || failure.Code != "action_not_authorized" {
		t.Fatalf("stale lease error/failure = %v/%+v", err, failure)
	}
	assertRuleCount(t, pool, 0)
}

func admitAndClaimRule(
	t *testing.T,
	pool *pgxpool.Pool,
	principal widgetauth.Principal,
	command LeadStatusRuleCommand,
	key string,
) jobs.Job {
	t.Helper()
	jobStore := jobs.NewStore(pool)
	result, err := NewActionStore(pool, jobStore).EnqueueLeadStatusRuleConfigure(
		context.Background(), principal, key, command,
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.Claim(context.Background(), "rule-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != result.JobID {
		t.Fatalf("claimed rule job/error = %+v/%v", claimed, err)
	}
	return claimed[0]
}

func completeRuleJob(t *testing.T, pool *pgxpool.Pool, job jobs.Job, result json.RawMessage) {
	t.Helper()
	if err := jobs.NewStore(pool).Complete(
		context.Background(), job, *job.LockedBy, result, time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
}

func assertRuleCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM lead_status_workflow_rules`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("rule count = %d, want %d", count, want)
	}
}

type staticRuleManagementAPI struct {
	mu     sync.Mutex
	admin  bool
	active bool
	calls  int
}

func (api *staticRuleManagementAPI) GetUserAuthorization(
	_ context.Context,
	_ uuid.UUID,
	userID int64,
) (amocrm.UserAuthorization, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.calls++
	var user amocrm.UserAuthorization
	user.ID = userID
	user.Rights.IsAdmin = api.admin
	user.Rights.IsActive = api.active
	return user, nil
}

func (api *staticRuleManagementAPI) Calls() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.calls
}

var _ RuleManagementAPI = (*staticRuleManagementAPI)(nil)
