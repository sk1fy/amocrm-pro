package webhook

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
)

func newWorkflowTestStore(pool *pgxpool.Pool) *Store {
	store := NewStore(pool)
	leadstatus.NewModule(pool, jobs.NewStore(pool)).RegisterEvents(store)
	return store
}
func testTransitionHandler(store *Store, execution *leadstatus.ExecutionStore, api leadstatus.LeadStatusTransitionAPI) jobs.Handler {
	return leadstatus.LeadStatusTransitionJobHandler(leadstatus.NewWorkflowStore(store.pool), execution, api)
}

type testTransitionPayload struct {
	WorkflowRunID    uuid.UUID `json:"workflow_run_id"`
	LeadID           int64     `json:"lead_id"`
	SourcePipelineID int64     `json:"source_pipeline_id"`
	SourceStatusID   int64     `json:"source_status_id"`
	PipelineID       int64     `json:"pipeline_id"`
	StatusID         int64     `json:"status_id"`
}
