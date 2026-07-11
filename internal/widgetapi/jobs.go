package widgetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

type LeadStatusAPI interface {
	GetUserAuthorization(context.Context, uuid.UUID, int64) (amocrm.UserAuthorization, error)
	GetLeadState(context.Context, uuid.UUID, int64) (amocrm.LeadState, error)
	PrepareLeadStatus(context.Context, uuid.UUID) (amocrm.LeadStatusMutation, error)
}

type LeadStatusResult struct {
	LeadID     int64 `json:"lead_id"`
	PipelineID int64 `json:"pipeline_id"`
	StatusID   int64 `json:"status_id"`
	Converged  bool  `json:"converged"`
}

func PingJobHandler(store *ExecutionStore) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if err := store.Authorize(ctx, job, PingJobType, ""); err != nil {
			return nil, jobs.Permanent("action_not_authorized", err)
		}
		userID, err := jobActorUserID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_actor", err)
		}
		metadata, _ := json.Marshal(map[string]string{"job_id": job.ID.String()})
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO audit_log (
				installation_id, actor_type, actor_id, action, object_type, object_id,
				correlation_job_id, metadata
			) VALUES ($1, 'widget_user', $2, 'widget.ping', 'job', $3, $4, $5)
			ON CONFLICT (correlation_job_id) DO NOTHING`,
			*job.InstallationID, strconv.FormatInt(userID, 10), job.ID.String(), job.ID, metadata,
		); err != nil {
			return nil, fmt.Errorf("audit widget ping: %w", err)
		}
		return json.RawMessage(`{"pong":true}`), nil
	}
}

func LeadSetStatusJobHandler(store *ExecutionStore, api LeadStatusAPI) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		installationID, err := installationID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_tenant_scope", err)
		}
		userID, err := jobActorUserID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_actor", err)
		}
		leadID, err := jobLeadID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_resource", err)
		}
		var command LeadStatusCommand
		if err := json.Unmarshal(job.Payload, &command); err != nil ||
			command.LeadID != leadID || command.PipelineID <= 0 || command.StatusID <= 0 {
			return nil, jobs.Permanent("invalid_payload", errors.New("lead status payload is invalid"))
		}
		if err := store.Authorize(ctx, job, LeadSetStatusJobType, leadResourceType); err != nil {
			return nil, jobs.Permanent("action_not_authorized", err)
		}

		user, err := api.GetUserAuthorization(ctx, installationID, userID)
		if err != nil {
			return nil, classifyWorkflowError(err)
		}
		if !user.Rights.IsActive || !user.Rights.IsAdmin {
			return nil, jobs.Permanent("actor_forbidden", errors.New("lead status workflow requires an active amoCRM administrator"))
		}
		lead, err := api.GetLeadState(ctx, installationID, leadID)
		if err != nil {
			return nil, classifyWorkflowError(err)
		}
		changed := lead.PipelineID != command.PipelineID || lead.StatusID != command.StatusID
		if changed {
			user, err = api.GetUserAuthorization(ctx, installationID, userID)
			if err != nil {
				return nil, classifyWorkflowError(err)
			}
			if !user.Rights.IsActive || !user.Rights.IsAdmin {
				return nil, jobs.Permanent("actor_forbidden", errors.New("lead status workflow requires an active amoCRM administrator"))
			}
			effectID, err := store.PrepareLeadStatusEffect(
				ctx, job, nil, leadID, command.PipelineID, command.StatusID,
			)
			if err != nil {
				return nil, err
			}
			mutation, err := api.PrepareLeadStatus(ctx, installationID)
			if err != nil {
				_ = store.MarkLeadStatusEffect(ctx, effectID, "failed", err)
				return nil, classifyWorkflowError(err)
			}
			if err := store.WithMutationAuthorization(
				ctx, job, LeadSetStatusJobType, leadResourceType,
				func(mutationContext context.Context) error {
					return mutation.SetLeadStatus(
						mutationContext, leadID, command.PipelineID, command.StatusID,
					)
				},
			); err != nil {
				if errors.Is(err, ErrExecutionNotAuthorized) {
					_ = store.MarkLeadStatusEffect(ctx, effectID, "failed", err)
					return nil, jobs.Permanent("action_not_authorized", err)
				}
				_ = store.MarkLeadStatusEffect(ctx, effectID, "uncertain", err)
				return nil, classifyMutationError(err)
			}
			if err := store.MarkLeadStatusEffect(ctx, effectID, "applied", nil); err != nil {
				return nil, err
			}
		} else if effectID, found, err := store.LeadStatusEffectForJob(ctx, job.ID); err != nil {
			return nil, err
		} else if found {
			if err := store.MarkLeadStatusEffect(ctx, effectID, "applied", nil); err != nil {
				return nil, err
			}
		}

		result := LeadStatusResult{
			LeadID: leadID, PipelineID: command.PipelineID,
			StatusID: command.StatusID, Converged: true,
		}
		if err := auditLeadStatus(ctx, store, job, userID, result); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("encode lead status result: %w", err)
		}
		return encoded, nil
	}
}

func classifyMutationError(err error) error {
	var apiError *amocrm.APIError
	if errors.As(err, &apiError) && apiError.Kind == amocrm.ErrorUnauthorized {
		return jobs.Retryable(string(apiError.Kind), 0, err)
	}
	return classifyWorkflowError(err)
}

func auditLeadStatus(
	ctx context.Context,
	store *ExecutionStore,
	job jobs.Job,
	userID int64,
	result LeadStatusResult,
) error {
	metadata, _ := json.Marshal(map[string]any{
		"job_id": job.ID, "pipeline_id": result.PipelineID,
		"status_id": result.StatusID, "outcome": "converged",
	})
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO audit_log (
			installation_id, actor_type, actor_id, action, object_type, object_id,
			correlation_job_id, metadata
		) VALUES ($1, 'widget_user', $2, 'workflow.lead.set_status', 'lead', $3, $4, $5)
		ON CONFLICT (correlation_job_id) DO NOTHING`,
		*job.InstallationID, strconv.FormatInt(userID, 10),
		strconv.FormatInt(result.LeadID, 10), job.ID, metadata,
	); err != nil {
		return fmt.Errorf("audit lead status workflow: %w", err)
	}
	return nil
}

func classifyWorkflowError(err error) error {
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
