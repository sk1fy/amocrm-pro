package maintenance

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
)

func TestCleanupSurvivesRuleConfigurationReceipt(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	_, installation := cleanupTenant(t, pool)
	ctx := context.Background()
	job := insertFinishedJob(t, pool, installation, "8 days")
	var rule uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO lead_status_workflow_rules(installation_id,source_pipeline_id,source_status_id,target_pipeline_id,target_status_id) VALUES($1,1,2,1,3) RETURNING id`, installation).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lead_status_workflow_rule_configurations(job_id,installation_id,rule_id,actor_user_id,source_pipeline_id,source_status_id,target_pipeline_id,target_status_id,enabled,revision) VALUES($1,$2,$3,77,1,2,1,3,true,1)`, job, installation, rule); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(pool).Cleanup(ctx, testPolicy(100, 2)); err != nil {
		t.Fatalf("normal completed rule job aborts entire maintenance transaction: %v", err)
	}
	var kept bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1)`, job).Scan(&kept); err != nil || !kept {
		t.Fatalf("young config lost its job: kept=%t err=%v", kept, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE lead_status_workflow_rule_configurations SET configured_at=now()-interval '8 days' WHERE job_id=$1`, job); err != nil {
		t.Fatal(err)
	}
	insertIdempotency(t, pool, installation, job, "long-lived-result", "10 days", "completed")
	result, err := NewStore(pool).Cleanup(ctx, testPolicy(100, 2))
	if err != nil || result.Jobs != 0 || result.RuleConfigurations != 0 {
		t.Fatalf("live receipt must protect result/job: %+v %v", result, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at=now()-interval '10 days',expires_at=now()-interval '1 day' WHERE job_id=$1`, job); err != nil {
		t.Fatal(err)
	}
	result, err = NewStore(pool).Cleanup(ctx, testPolicy(1, 1))
	if err != nil || result.RuleConfigurations != 1 || result.Jobs != 1 {
		t.Fatalf("expired chain did not collect in dependency order: %+v %v", result, err)
	}
}
