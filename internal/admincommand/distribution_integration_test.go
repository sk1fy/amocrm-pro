package admincommand

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/distribution"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"testing"
)

func TestDistributionPauseAtomicReceiptActorAndAudit(t *testing.T) {
	s, pool, id, _ := fixture(t)
	req := request("installation", id, "distribution-pause")
	req.Payload = json.RawMessage(`{"expected_paused":false}`)
	key := uuid.NewString()
	first, e := s.Execute(t.Context(), fixtureActor, key, req)
	if e != nil || first.State != "succeeded" {
		t.Fatal(first, e)
	}
	replay, e := s.Execute(t.Context(), fixtureActor, key, req)
	if e != nil || replay.ID != first.ID {
		t.Fatal(replay, e)
	}
	if _, e = s.Execute(t.Context(), "employee:other", key, req); e == nil {
		t.Fatal("other actor replay permitted")
	}
	req.Payload = []byte(`{"expected_paused":true}`)
	if _, e = s.Execute(t.Context(), fixtureActor, key, req); e == nil {
		t.Fatal("different payload replay permitted")
	}
	var paused bool
	var audits int
	if e = pool.QueryRow(t.Context(), `SELECT paused FROM distribution_admin_pauses WHERE installation_id=$1`, id).Scan(&paused); e != nil || !paused {
		t.Fatal(paused, e)
	}
	if e = pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor_id=$1 AND action='admin.distribution-pause'`, fixtureActor).Scan(&audits); e != nil {
		t.Fatal(e)
	}
	// Existing canonical audit uses the admin.command action; verify receipt identity.
	if audits == 0 {
		if e = pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor_id=$1 AND metadata->>'command'='distribution-pause'`, fixtureActor).Scan(&audits); e != nil {
			t.Fatal(e)
		}
	}
	if audits != 1 {
		t.Fatal("missing/duplicate actor audit", audits)
	}
	if _, e = s.Execute(t.Context(), fixtureActor, uuid.NewString(), request("installation", id, "distribution-reconcile")); e == nil {
		t.Fatal("missing evidence accepted")
	}
}

type adminReadOnlyCRM struct{ calls int }

func (c *adminReadOnlyCRM) GetLeadSnapshot(context.Context, uuid.UUID, int64) (amocrm.LeadState, error) {
	c.calls++
	return amocrm.LeadState{ID: 99, PipelineID: 10, StatusID: 20, ResponsibleUserID: 2, UpdatedAt: 100}, nil
}
func (c *adminReadOnlyCRM) PrepareLeadResponsible(context.Context, uuid.UUID) (amocrm.LeadResponsibleMutation, error) {
	return nil, errors.New("PATCH forbidden in admin verification")
}
func (c *adminReadOnlyCRM) DistributionUser(context.Context, uuid.UUID, int64) (amocrm.DistributionUser, error) {
	return amocrm.DistributionUser{}, errors.New("unexpected user lookup")
}
func TestDistributionAdminWorkerObservesUnknownWithoutAssignmentSuccess(t *testing.T) {
	s, pool, installation, integration := fixture(t)
	binding, company, operation, assignmentJob := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	command, _ := json.Marshal(map[string]any{"scope": map[string]any{"companyId": company, "installationId": installation, "integrationId": integration, "accountId": "91030303", "bindingId": binding, "bindingRevision": 1}, "command": map[string]any{"expectedSnapshot": map[string]any{"leadId": "99", "pipelineId": "10", "statusId": "20", "responsibleUserId": "1"}, "targetResponsibleUserId": "2"}})
	for _, stmt := range []struct {
		q string
		a []any
	}{
		{`INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,91030303,1,$5,1)`, []any{binding, company, installation, integration, uuid.New()}},
		{`INSERT INTO jobs(id,installation_id,type,payload) VALUES($1,$2,'distribution.assign_responsible','{}')`, []any{assignmentJob, installation}},
		{`INSERT INTO distribution_operations(id,binding_id,company_id,installation_id,integration_id,account_id,binding_revision,decision_id,lead_id,key_hash,request_hash,command,receipt,job_id,state,external_effect_state) VALUES($1,$2,$3,$4,$5,91030303,1,$6,99,decode(repeat('00',32),'hex'),decode(repeat('00',32),'hex'),$7,'{}',$8,'outcome_unknown','unknown')`, []any{operation, binding, company, installation, integration, uuid.New(), command, assignmentJob}},
		{`INSERT INTO distribution_lead_guards(account_id,lead_id,operation_id) VALUES(91030303,99,$1)`, []any{operation}},
		{`INSERT INTO distribution_operation_attempts(operation_id,job_id,fence,executor) VALUES($1,$2,1,'fixture')`, []any{operation, assignmentJob}},
	} {
		if _, e := pool.Exec(t.Context(), stmt.q, stmt.a...); e != nil {
			t.Fatal(e)
		}
	}
	req := request("installation", installation, "distribution-reconcile")
	req.Payload, _ = json.Marshal(map[string]any{"operation_id": operation, "expected_result_version": 1})
	receipt, e := s.Execute(t.Context(), fixtureActor, uuid.NewString(), req)
	if e != nil || receipt.State != "pending" || receipt.JobID == nil {
		t.Fatal(receipt, e)
	}
	var payload []byte
	if e = pool.QueryRow(t.Context(), `SELECT payload FROM jobs WHERE id=$1`, receipt.JobID).Scan(&payload); e != nil {
		t.Fatal(e)
	}
	actorType, actor, resource, resourceID := "admin", fixtureActor, "admin_command", receipt.ID.String()
	job := jobs.Job{ID: *receipt.JobID, InstallationID: &installation, Type: DistributionReconcileJobType, ActorType: &actorType, ActorID: &actor, ResourceType: &resource, ResourceID: &resourceID, Payload: payload}
	crm := &adminReadOnlyCRM{}
	executor := WorkerExecutor{pool: pool, Distribution: &distribution.AssignmentWorker{Store: distribution.NewStore(pool), CRM: crm}}
	raw, e := executor.Handler(t.Context(), job)
	if e != nil {
		t.Fatal(e)
	}
	var result executionResult
	if e = json.Unmarshal(raw, &result); e != nil || result.State != "succeeded" || result.Outcome != "observed" || result.Result["assignment_state"] != "outcome_unknown" {
		t.Fatal(string(raw), e)
	}
	if _, e = executor.Handler(t.Context(), job); e != nil || crm.calls != 1 {
		t.Fatal("same command reread or changed effect", crm.calls, e)
	}
	forged := "employee:forged"
	job.ActorID = &forged
	if _, e = executor.Handler(t.Context(), job); e == nil {
		t.Fatal("forged actor executed")
	}
	var guards, audits int
	if e = pool.QueryRow(t.Context(), `SELECT count(*) FROM distribution_lead_guards WHERE operation_id=$1`, operation).Scan(&guards); e != nil || guards != 1 {
		t.Fatal("unknown guard released", guards, e)
	}
	if e = pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor_type='admin' AND actor_id=$1 AND action='distribution.reconcile'`, fixtureActor).Scan(&audits); e != nil || audits != 1 {
		t.Fatal("owner admin audit missing/repeated", audits, e)
	}
}
