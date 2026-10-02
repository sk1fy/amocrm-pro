package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"sync"
	"testing"
	"time"
)

type assignmentFixtureData struct {
	Store      *Store
	Pool       *pgxpool.Pool
	Scope      Scope
	Assignment Assignment
	CRM        *assignmentFakeCRM
	Policy     *assignmentFakePolicy
	Jobs       *jobs.Store
}

func assignmentFixture(t *testing.T) *assignmentFixtureData {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integration, install, company, binding, employee := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,'x','https://service.test/oauth')`, []any{integration, uuid.NewString(), uuid.NewString()}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'test.amocrm.ru','active')`, []any{install, integration}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)`, []any{integration}},
		{`INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,123,1,$5,1)`, []any{binding, company, install, integration, uuid.New()}},
		{`INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,2)`, []any{binding, employee}},
		{`INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES('service',$1,$2,true)`, []any{company, install}},
	} {
		if _, e := pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now().UTC()
	updated := time.Unix(100, 0).UTC()
	a := Assignment{SchemaVersion: 1, MessageID: uuid.New(), Scope: AssignmentScope{company, install, integration, 123, binding, 1}, EventID: uuid.New(), SourceOccurredAt: now, ReceivedAt: now, EmittedAt: now, CorrelationID: uuid.New(), CausationID: uuid.New(), Command: AssignmentCommand{OperationID: uuid.New(), EpisodeID: uuid.New(), DecisionID: uuid.New(), RuleID: uuid.New(), GroupID: uuid.New(), RuleRevision: 1, AvailabilityRevision: 1, ClaimRevision: 1, DecisionKind: "assign", TargetResponsibleUserID: 2, Expected: Snapshot{10, 20, 30, 1, &updated, now}, Actor: Actor{Kind: "system"}, ValidUntil: now.Add(10 * time.Minute)}}
	crm := &assignmentFakeCRM{crmFake: crmFake{users: map[int64]amocrm.DistributionUser{1: {ID: 1, Rights: amocrm.DistributionRights{IsActive: ptr(true), Leads: map[string]string{"view": "A"}}}, 2: {ID: 2, Rights: amocrm.DistributionRights{IsActive: ptr(true), Leads: map[string]string{"view": "A"}}}}}, Lead: amocrm.LeadState{ID: 10, Name: "Synthetic", PipelineID: 20, StatusID: 30, ResponsibleUserID: 1, UpdatedAt: 100}}
	return &assignmentFixtureData{NewStore(pool), pool, Scope{"service", company, install}, a, crm, &assignmentFakePolicy{}, jobs.NewStore(pool)}
}

type assignmentFakePolicy struct {
	Deny    bool
	Err     error
	Expired bool
}

func (p *assignmentFakePolicy) ValidateDecision(_ context.Context, op Operation, _ uuid.UUID, fence int) (DecisionAuthorization, error) {
	until := time.Now().Add(3 * time.Second)
	if p.Expired {
		until = time.Now().Add(-time.Second)
	}
	return DecisionAuthorization{!p.Deny, op.OperationID, op.DecisionID, fence, until, "synthetic_decision_evidence"}, p.Err
}

type assignmentFakeCRM struct {
	crmFake
	mu          sync.Mutex
	Lead        amocrm.LeadState
	Mode        string
	Calls       int
	PrepareErr  error
	ObserveErr  error
	AckUpdated  int64
	PrepareHook func()
}

func (f *assignmentFakeCRM) GetLeadSnapshot(context.Context, uuid.UUID, int64) (amocrm.LeadState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Lead, f.ObserveErr
}
func (f *assignmentFakeCRM) PrepareLeadResponsible(context.Context, uuid.UUID) (amocrm.LeadResponsibleMutation, error) {
	if f.PrepareHook != nil {
		f.PrepareHook()
	}
	return f, f.PrepareErr
}
func (f *assignmentFakeCRM) Assign(ctx context.Context, leadID, target int64) (amocrm.LeadResponsibleResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Mode == "before_send" {
		return amocrm.LeadResponsibleResult{}, &amocrm.ResponsibleDispatchError{Cause: context.Canceled}
	}
	if f.Mode == "timeout_unapplied" {
		return amocrm.LeadResponsibleResult{}, &amocrm.ResponsibleDispatchError{Dispatched: true, Cause: context.DeadlineExceeded}
	}
	f.Lead.ResponsibleUserID = target
	f.Lead.UpdatedAt++
	ack := f.Lead.UpdatedAt
	f.AckUpdated = ack
	if f.Mode == "timeout_applied" {
		return amocrm.LeadResponsibleResult{}, &amocrm.ResponsibleDispatchError{Dispatched: true, Cause: context.DeadlineExceeded}
	}
	if f.Mode == "manual_after_ack" {
		f.Lead.ResponsibleUserID = 3
		f.Lead.UpdatedAt++
	}
	if f.Mode == "stale_observation" {
		f.Lead.UpdatedAt--
	}
	if f.Mode == "observe_failure" {
		f.ObserveErr = errors.New("source unavailable")
	}
	return amocrm.LeadResponsibleResult{HTTPStatus: 200, Accepted: true, UpdatedAt: ack}, nil
}
func (f *assignmentFixtureData) AdmitAndClaim(t *testing.T) jobs.Job {
	t.Helper()
	if _, _, e := f.Store.Admit(context.Background(), f.Scope, uuid.NewString(), f.Assignment); e != nil {
		t.Fatal(e)
	}
	jobs, e := f.Jobs.Claim(context.Background(), "worker", 1, time.Minute)
	if e != nil || len(jobs) != 1 {
		t.Fatal(jobs, e)
	}
	return jobs[0]
}
func (f *assignmentFixtureData) Worker() *AssignmentWorker {
	return &AssignmentWorker{f.Store, f.CRM, f.Policy}
}
