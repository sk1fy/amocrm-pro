package admincommand

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
)

func TestLeadStatusMutationRollsBackWhenAdminReceiptAuditFails(t *testing.T) {
	s, pool, installationID, _ := fixture(t)
	s.rules = leadstatus.NewRuleStore(pool)
	// Fail after the domain savepoint succeeds, at the outer receipt audit.
	// An independently committed RuleStore transaction would survive this.
	const removeFault = `DROP TRIGGER IF EXISTS reject_rule_receipt_audit ON audit_log;
		DROP FUNCTION IF EXISTS reject_rule_receipt_audit()`
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION reject_rule_receipt_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.action='admin.command.succeeded' AND NEW.metadata->>'command'='lead-status-configure' THEN
				RAISE EXCEPTION 'fixture receipt audit failure';
			END IF;
			RETURN NEW;
		END$$;
		CREATE TRIGGER reject_rule_receipt_audit BEFORE INSERT ON audit_log
		FOR EACH ROW EXECUTE FUNCTION reject_rule_receipt_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), removeFault) })
	key := uuid.NewString()
	req := request("installation", installationID, "lead-status-configure")
	req.Payload = json.RawMessage(`{"source_pipeline_id":1,"source_status_id":2,"target_pipeline_id":3,"target_status_id":4,"enabled":true,"expected_revision":0}`)
	if _, err := s.Execute(t.Context(), fixtureActor, key, req); err == nil {
		t.Fatal("receipt audit failure was not returned")
	}
	for _, query := range []string{
		`SELECT count(*) FROM lead_status_workflow_rules WHERE installation_id=$1`,
		`SELECT count(*) FROM lead_status_workflow_rule_configurations WHERE installation_id=$1`,
		`SELECT count(*) FROM jobs WHERE installation_id=$1 AND actor_type='admin'`,
		`SELECT count(*) FROM audit_log WHERE installation_id=$1 AND actor_type='admin'`,
	} {
		var count int
		if err := pool.QueryRow(t.Context(), query, installationID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("domain mutation survived rollback: count=%d, error=%v, query=%s", count, err, query)
		}
	}
	var receiptCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_commands WHERE id=$1`, key).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatalf("receipt survived rollback: count=%d, error=%v", receiptCount, err)
	}
	if _, err := pool.Exec(t.Context(), removeFault); err != nil {
		t.Fatal(err)
	}
	first, err := s.Execute(t.Context(), fixtureActor, key, req)
	if err != nil || first.State != "succeeded" {
		t.Fatalf("retry after rolled-back transaction: state=%s, error=%v", first.State, err)
	}
	second, err := s.Execute(t.Context(), "employee:another", key, req)
	if err != nil || second.ID != first.ID || second.State != "succeeded" {
		t.Fatalf("replay: state=%s, same ID=%v, error=%v", second.State, second.ID == first.ID, err)
	}
	var revision int64
	if err := pool.QueryRow(t.Context(), `SELECT revision FROM lead_status_workflow_rules WHERE installation_id=$1`, installationID).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("replay mutated rule: revision=%d, error=%v", revision, err)
	}
	var jobsCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE installation_id=$1 AND actor_type='admin'`, installationID).Scan(&jobsCount); err != nil || jobsCount != 1 {
		t.Fatalf("replay created a second job: count=%d, error=%v", jobsCount, err)
	}
}
