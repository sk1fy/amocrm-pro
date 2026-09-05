package leadstatus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

func (s *WorkflowStore) RecordJobFailure(ctx context.Context, tx jobs.TxExecutor, job jobs.Job, failure jobs.Failure, status jobs.Status) error {
	if job.InstallationID == nil || job.Type != LeadStatusTransitionJobType {
		return nil
	}
	var payload leadStatusTransitionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.WorkflowRunID == uuid.Nil {
		return nil
	}
	runStatus := "queued"
	terminal := false
	if status == jobs.StatusFailed {
		runStatus, terminal = "failed", true
	} else if status == jobs.StatusDead {
		runStatus, terminal = "dead", true
	}
	if _, err := tx.Exec(ctx, `
			UPDATE workflow_runs
			SET status=$4, finished_at=CASE WHEN $5 THEN now() ELSE NULL END
			WHERE id=$1 AND installation_id=$2 AND job_id=$3 AND status <> 'completed'`,
		payload.WorkflowRunID, *job.InstallationID, job.ID, runStatus, terminal,
	); err != nil {
		return fmt.Errorf("record workflow run failure: %w", err)
	}
	if _, err := tx.Exec(ctx, `
			UPDATE outbound_effects
			SET state=CASE
					WHEN state='prepared' THEN 'uncertain'
					ELSE state
				END,
				last_error=$2, updated_at=now()
			WHERE correlation_job_id=$1`,
		job.ID, sanitize.Text(failure.Message, 4000),
	); err != nil {
		return fmt.Errorf("record workflow effect failure: %w", err)
	}
	return nil
}
