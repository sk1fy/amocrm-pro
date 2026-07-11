package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

const (
	LeadStatusTransitionJobType = "workflow.lead.status_transition"
	leadStatusWorkflowType      = "lead.status_transition"
	leadStatusWorkflowVersion   = 1
	leadStatusEffectType        = "lead.set_status"
	leadResourceType            = "lead"
)

var errLeadStatusWorkflowRunMismatch = errors.New("lead status workflow run does not match its job and tenant")

type leadStatusEvent struct {
	ID               uuid.UUID
	InstallationID   uuid.UUID
	EntityType       string
	EventType        string
	EntityID         *int64
	Payload          json.RawMessage
	DeduplicationKey []byte
	ReceivedAt       time.Time
}

type eventRoute struct {
	Disposition string
	RunID       *uuid.UUID
	EffectID    *uuid.UUID
}

type leadStatusTransitionPayload struct {
	WorkflowRunID uuid.UUID `json:"workflow_run_id"`
	LeadID        int64     `json:"lead_id"`
	PipelineID    int64     `json:"pipeline_id"`
	StatusID      int64     `json:"status_id"`
}

type leadStatusEventState struct {
	PipelineID int64
	StatusID   int64
}

type LeadStatusTransitionAPI interface {
	GetLeadState(context.Context, uuid.UUID, int64) (amocrm.LeadState, error)
	PrepareLeadStatus(context.Context, uuid.UUID) (amocrm.LeadStatusMutation, error)
}

func (s *Store) routeLeadStatusEvent(
	ctx context.Context,
	tx pgx.Tx,
	event leadStatusEvent,
) (eventRoute, error) {
	if event.EntityType != "leads" || event.EventType != "status" || event.EntityID == nil {
		return eventRoute{Disposition: "observed"}, nil
	}
	state, ok := decodeLeadStatusEvent(event.Payload)
	if !ok {
		return eventRoute{Disposition: "invalid_status_event"}, nil
	}
	desiredHash := widgetapi.LeadStatusEffectHash(state.PipelineID, state.StatusID)
	var effectID uuid.UUID
	err := tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM outbound_effects
			WHERE installation_id=$1 AND effect_type=$2
			  AND resource_type='lead' AND resource_id=$3 AND desired_hash=$4
			  AND state IN ('prepared', 'applied', 'uncertain')
			  AND attempted_at <= $5
			  AND correlation_expires_at >= $5
			ORDER BY attempted_at DESC
			LIMIT 1
			FOR UPDATE
		)
		UPDATE outbound_effects AS effect
		SET state='observed', observed_at=now(),
			correlated_event_deduplication_key=$6, updated_at=now()
		FROM candidate
		WHERE effect.id=candidate.id
		RETURNING effect.id`,
		event.InstallationID, leadStatusEffectType,
		strconv.FormatInt(*event.EntityID, 10), desiredHash[:], event.ReceivedAt,
		event.DeduplicationKey,
	).Scan(&effectID)
	if err == nil {
		return eventRoute{Disposition: "self_effect", EffectID: &effectID}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return eventRoute{}, fmt.Errorf("correlate lead status effect: %w", err)
	}

	var ruleID uuid.UUID
	var targetPipelineID, targetStatusID int64
	err = tx.QueryRow(ctx, `
		SELECT id, target_pipeline_id, target_status_id
		FROM lead_status_workflow_rules
		WHERE installation_id=$1 AND source_pipeline_id=$2 AND source_status_id=$3
		  AND enabled`,
		event.InstallationID, state.PipelineID, state.StatusID,
	).Scan(&ruleID, &targetPipelineID, &targetStatusID)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventRoute{Disposition: "observed"}, nil
	}
	if err != nil {
		return eventRoute{}, fmt.Errorf("load lead status workflow rule: %w", err)
	}

	var runID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO workflow_runs (
			installation_id, workflow_type, workflow_version,
			origin_deduplication_key, origin_event_id, rule_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (
			installation_id, workflow_type, workflow_version,
			origin_deduplication_key
		) DO NOTHING
		RETURNING id`,
		event.InstallationID, leadStatusWorkflowType, leadStatusWorkflowVersion,
		event.DeduplicationKey, event.ID, ruleID,
	).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventRoute{Disposition: "duplicate_workflow"}, nil
	}
	if err != nil {
		return eventRoute{}, fmt.Errorf("create lead status workflow run: %w", err)
	}
	payload := leadStatusTransitionPayload{
		WorkflowRunID: runID, LeadID: *event.EntityID,
		PipelineID: targetPipelineID, StatusID: targetStatusID,
	}
	job, err := jobs.NewStore(s.pool).EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &event.InstallationID,
		Type:           LeadStatusTransitionJobType, ActorType: "integration",
		ActorID: event.InstallationID.String(), ResourceType: leadResourceType,
		ResourceID: strconv.FormatInt(*event.EntityID, 10), Priority: 30,
		MaxAttempts: 5, Payload: payload,
	})
	if err != nil {
		return eventRoute{}, fmt.Errorf("enqueue lead status workflow: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET job_id=$2 WHERE id=$1`, runID, job.ID); err != nil {
		return eventRoute{}, fmt.Errorf("link lead status workflow job: %w", err)
	}
	return eventRoute{Disposition: "workflow_enqueued", RunID: &runID}, nil
}

func decodeLeadStatusEvent(payload json.RawMessage) (leadStatusEventState, bool) {
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		return leadStatusEventState{}, false
	}
	pipelineID, ok := positiveJSONInt(fields["pipeline_id"])
	if !ok {
		return leadStatusEventState{}, false
	}
	statusID, ok := positiveJSONInt(fields["status_id"])
	return leadStatusEventState{PipelineID: pipelineID, StatusID: statusID}, ok
}

func positiveJSONInt(value any) (int64, bool) {
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		raw = strconv.FormatInt(int64(typed), 10)
	default:
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	return parsed, err == nil && parsed > 0
}

func (s *Store) completeLeadStatusRun(
	ctx context.Context,
	job jobs.Job,
	payload leadStatusTransitionPayload,
	outcome string,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete lead status workflow: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_runs SET status='completed', finished_at=COALESCE(finished_at, now())
		WHERE id=$1 AND installation_id=$2 AND job_id=$3`,
		payload.WorkflowRunID, *job.InstallationID, job.ID,
	); err != nil {
		return fmt.Errorf("complete lead status workflow run: %w", err)
	}
	metadata, _ := json.Marshal(map[string]any{
		"workflow_run_id": payload.WorkflowRunID,
		"pipeline_id":     payload.PipelineID, "status_id": payload.StatusID,
		"outcome": outcome,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (
			installation_id, actor_type, actor_id, action, object_type, object_id,
			correlation_job_id, metadata
		) VALUES ($1, 'integration', $2, $3, 'lead', $4, $5, $6)
		ON CONFLICT (correlation_job_id) DO NOTHING`,
		*job.InstallationID, job.InstallationID.String(), LeadStatusTransitionJobType,
		strconv.FormatInt(payload.LeadID, 10), job.ID, metadata,
	); err != nil {
		return fmt.Errorf("audit lead status workflow: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Store) markLeadStatusRunProcessing(
	ctx context.Context,
	job jobs.Job,
	payload leadStatusTransitionPayload,
) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workflow_runs
		SET status='processing'
		WHERE id=$1 AND installation_id=$2 AND job_id=$3
		  AND workflow_type=$4 AND workflow_version=$5
		  AND status IN ('queued', 'processing')`,
		payload.WorkflowRunID, *job.InstallationID, job.ID,
		leadStatusWorkflowType, leadStatusWorkflowVersion,
	)
	if err != nil {
		return fmt.Errorf("mark lead status workflow processing: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errLeadStatusWorkflowRunMismatch
	}
	return nil
}

// LeadStatusTransitionJobHandler executes the PostgreSQL-originated transition
// rule. The caller wires it under LeadStatusTransitionJobType.
func LeadStatusTransitionJobHandler(
	store *Store,
	execution *widgetapi.ExecutionStore,
	api LeadStatusTransitionAPI,
) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if job.InstallationID == nil || job.ResourceType == nil || job.ResourceID == nil {
			return nil, jobs.Permanent("invalid_tenant_scope", errors.New("workflow job ownership is incomplete"))
		}
		var payload leadStatusTransitionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil ||
			payload.WorkflowRunID == uuid.Nil || payload.LeadID <= 0 ||
			payload.PipelineID <= 0 || payload.StatusID <= 0 ||
			*job.ResourceType != leadResourceType ||
			*job.ResourceID != strconv.FormatInt(payload.LeadID, 10) {
			return nil, jobs.Permanent("invalid_payload", errors.New("lead status transition payload is invalid"))
		}
		if err := execution.AuthorizeIntegrationAction(
			ctx, job, LeadStatusTransitionJobType, leadResourceType,
		); err != nil {
			if errors.Is(err, widgetapi.ErrExecutionNotAuthorized) {
				return nil, jobs.Permanent("action_not_authorized", err)
			}
			return nil, err
		}
		if err := store.markLeadStatusRunProcessing(ctx, job, payload); err != nil {
			if errors.Is(err, errLeadStatusWorkflowRunMismatch) {
				return nil, jobs.Permanent("invalid_workflow_run", err)
			}
			return nil, err
		}
		lead, err := api.GetLeadState(ctx, *job.InstallationID, payload.LeadID)
		if err != nil {
			return nil, classifyLeadStatusWorkflowError(err)
		}
		if lead.PipelineID == payload.PipelineID && lead.StatusID == payload.StatusID {
			if effectID, found, err := execution.LeadStatusEffectForJob(ctx, job.ID); err != nil {
				return nil, err
			} else if found {
				if err := execution.MarkLeadStatusEffect(ctx, effectID, "applied", nil); err != nil {
					return nil, err
				}
			}
			if err := store.completeLeadStatusRun(ctx, job, payload, "already_converged"); err != nil {
				return nil, err
			}
			return leadStatusTransitionResult(payload), nil
		}

		effectID, err := execution.PrepareLeadStatusEffect(
			ctx, job, &payload.WorkflowRunID, payload.LeadID, payload.PipelineID, payload.StatusID,
		)
		if err != nil {
			return nil, err
		}
		mutation, err := api.PrepareLeadStatus(ctx, *job.InstallationID)
		if err != nil {
			_ = execution.MarkLeadStatusEffect(ctx, effectID, "failed", err)
			return nil, classifyLeadStatusWorkflowError(err)
		}
		err = execution.WithIntegrationMutationAuthorization(
			ctx, job, LeadStatusTransitionJobType, leadResourceType,
			func(mutationContext context.Context) error {
				return mutation.SetLeadStatus(
					mutationContext, payload.LeadID, payload.PipelineID, payload.StatusID,
				)
			},
		)
		if err != nil {
			if errors.Is(err, widgetapi.ErrExecutionNotAuthorized) {
				_ = execution.MarkLeadStatusEffect(ctx, effectID, "failed", err)
				return nil, jobs.Permanent("action_not_authorized", err)
			}
			_ = execution.MarkLeadStatusEffect(ctx, effectID, "uncertain", err)
			return nil, classifyLeadStatusWorkflowError(err)
		}
		if err := execution.MarkLeadStatusEffect(ctx, effectID, "applied", nil); err != nil {
			return nil, err
		}
		if err := store.completeLeadStatusRun(ctx, job, payload, "converged"); err != nil {
			return nil, err
		}
		return leadStatusTransitionResult(payload), nil
	}
}

func leadStatusTransitionResult(payload leadStatusTransitionPayload) json.RawMessage {
	result, _ := json.Marshal(map[string]any{
		"lead_id": payload.LeadID, "pipeline_id": payload.PipelineID,
		"status_id": payload.StatusID, "converged": true,
	})
	return result
}

func classifyLeadStatusWorkflowError(err error) error {
	var apiError *amocrm.APIError
	if errors.As(err, &apiError) && apiError.Retryable {
		return jobs.Retryable(string(apiError.Kind), apiError.RetryAfter, err)
	}
	if errors.As(err, &apiError) {
		return jobs.Permanent(string(apiError.Kind), err)
	}
	if errors.Is(err, amocrm.ErrIncompleteResponse) {
		return jobs.Permanent("invalid_amocrm_response", err)
	}
	return jobs.Retryable("amocrm_request_failed", 5*time.Second, err)
}
