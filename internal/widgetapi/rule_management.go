package widgetapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

var ErrRuleRevisionConflict = errors.New("lead status rule revision conflict")

type LeadStatusRuleCommand struct {
	SourcePipelineID int64 `json:"source_pipeline_id"`
	SourceStatusID   int64 `json:"source_status_id"`
	TargetPipelineID int64 `json:"target_pipeline_id"`
	TargetStatusID   int64 `json:"target_status_id"`
	Enabled          bool  `json:"enabled"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type LeadStatusRuleResult struct {
	RuleID           uuid.UUID `json:"rule_id"`
	SourcePipelineID int64     `json:"source_pipeline_id"`
	SourceStatusID   int64     `json:"source_status_id"`
	TargetPipelineID int64     `json:"target_pipeline_id"`
	TargetStatusID   int64     `json:"target_status_id"`
	Enabled          bool      `json:"enabled"`
	Revision         int64     `json:"revision"`
}

type RuleManagementAPI interface {
	GetUserAuthorization(context.Context, uuid.UUID, int64) (amocrm.UserAuthorization, error)
}

type RuleStore struct{ pool *pgxpool.Pool }

func NewRuleStore(pool *pgxpool.Pool) *RuleStore { return &RuleStore{pool: pool} }

func (s *ActionStore) EnqueueLeadStatusRuleConfigure(
	ctx context.Context,
	principal widgetauth.Principal,
	idempotencyKey string,
	command LeadStatusRuleCommand,
) (ActionResult, error) {
	if !validLeadStatusRuleCommand(command) {
		return ActionResult{}, ErrInvalidLeadStatusRule
	}
	return s.enqueue(ctx, actionAdmission{
		principal: principal, idempotencyKey: idempotencyKey,
		scope: leadStatusRuleScope, requestHash: leadStatusRuleRequestHash(principal, command),
		jobType: LeadStatusRuleConfigureJobType, resourceType: leadStatusRuleResourceType,
		resourceID: leadStatusRuleResourceID(command), payload: command,
		priority: 35, maxAttempts: 5,
	})
}

func validLeadStatusRuleCommand(command LeadStatusRuleCommand) bool {
	return command.SourcePipelineID > 0 && command.SourceStatusID > 0 &&
		command.TargetPipelineID > 0 && command.TargetStatusID > 0 &&
		command.ExpectedRevision >= 0 &&
		(command.SourcePipelineID != command.TargetPipelineID ||
			command.SourceStatusID != command.TargetStatusID)
}

func leadStatusRuleResourceID(command LeadStatusRuleCommand) string {
	return strconv.FormatInt(command.SourcePipelineID, 10) + ":" +
		strconv.FormatInt(command.SourceStatusID, 10)
}

func leadStatusRuleRequestHash(
	principal widgetauth.Principal,
	command LeadStatusRuleCommand,
) [sha256.Size]byte {
	canonical := fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%s\x00%d\x00%d\x00%d\x00%d\x00%t\x00%d",
		leadStatusRuleScope, principal.InstallationID, principal.AccountID,
		principal.UserID, principal.ClientUUID, command.SourcePipelineID,
		command.SourceStatusID, command.TargetPipelineID, command.TargetStatusID,
		command.Enabled, command.ExpectedRevision,
	)
	return sha256.Sum256([]byte(canonical))
}

func LeadStatusRuleConfigureJobHandler(
	execution *ExecutionStore,
	rules *RuleStore,
	api RuleManagementAPI,
) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		installationID, err := installationID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_tenant_scope", err)
		}
		userID, err := jobActorUserID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_actor", err)
		}
		var command LeadStatusRuleCommand
		if err := json.Unmarshal(job.Payload, &command); err != nil ||
			!validLeadStatusRuleCommand(command) || job.ResourceType == nil ||
			job.ResourceID == nil || *job.ResourceType != leadStatusRuleResourceType ||
			*job.ResourceID != leadStatusRuleResourceID(command) {
			return nil, jobs.Permanent("invalid_payload", errors.New("lead status rule payload is invalid"))
		}
		if result, found, err := rules.ResultForJob(ctx, job.ID, installationID); err != nil {
			return nil, err
		} else if found {
			return encodeLeadStatusRuleResult(result)
		}
		if err := execution.Authorize(ctx, job, LeadStatusRuleConfigureJobType, leadStatusRuleResourceType); err != nil {
			return nil, jobs.Permanent("action_not_authorized", err)
		}
		user, err := api.GetUserAuthorization(ctx, installationID, userID)
		if err != nil {
			return nil, classifyWorkflowError(err)
		}
		if !user.Rights.IsActive || !user.Rights.IsAdmin {
			return nil, jobs.Permanent("actor_forbidden", errors.New("rule configuration requires an active amoCRM administrator"))
		}
		result, err := rules.Configure(ctx, job, userID, command)
		if errors.Is(err, ErrRuleRevisionConflict) {
			return nil, jobs.Permanent("revision_conflict", err)
		}
		if errors.Is(err, ErrExecutionNotAuthorized) {
			return nil, jobs.Permanent("action_not_authorized", err)
		}
		if err != nil {
			return nil, err
		}
		return encodeLeadStatusRuleResult(result)
	}
}

func encodeLeadStatusRuleResult(result LeadStatusRuleResult) (json.RawMessage, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode lead status rule result: %w", err)
	}
	return encoded, nil
}

func (s *RuleStore) ResultForJob(
	ctx context.Context,
	jobID uuid.UUID,
	installationID uuid.UUID,
) (LeadStatusRuleResult, bool, error) {
	if s == nil || s.pool == nil {
		return LeadStatusRuleResult{}, false, errors.New("rule store is not configured")
	}
	result, err := scanRuleResult(s.pool.QueryRow(ctx, `
		SELECT rule_id,source_pipeline_id,source_status_id,target_pipeline_id,
			target_status_id,enabled,revision
		FROM lead_status_workflow_rule_configurations
		WHERE job_id=$1 AND installation_id=$2`, jobID, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadStatusRuleResult{}, false, nil
	}
	if err != nil {
		return LeadStatusRuleResult{}, false, fmt.Errorf("load rule configuration receipt: %w", err)
	}
	return result, true, nil
}

func (s *RuleStore) Configure(
	ctx context.Context,
	job jobs.Job,
	userID int64,
	command LeadStatusRuleCommand,
) (LeadStatusRuleResult, error) {
	if s == nil || s.pool == nil || job.InstallationID == nil || job.LockedBy == nil ||
		job.LockedUntil == nil || job.ActorType == nil || job.ActorID == nil ||
		job.ResourceType == nil || job.ResourceID == nil || userID <= 0 {
		return LeadStatusRuleResult{}, ErrExecutionNotAuthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("begin rule configuration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if result, err := scanRuleResult(tx.QueryRow(ctx, `
		SELECT rule_id,source_pipeline_id,source_status_id,target_pipeline_id,
			target_status_id,enabled,revision
		FROM lead_status_workflow_rule_configurations
		WHERE job_id=$1 AND installation_id=$2`, job.ID, *job.InstallationID)); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return LeadStatusRuleResult{}, fmt.Errorf("commit existing rule receipt: %w", err)
		}
		return result, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return LeadStatusRuleResult{}, fmt.Errorf("load rule receipt for update: %w", err)
	}

	var marker int
	err = tx.QueryRow(ctx, `
		SELECT 1
		FROM jobs AS job
		JOIN installations AS installation ON installation.id=job.installation_id
		JOIN integrations AS integration ON integration.id=installation.integration_id
		WHERE job.id=$1 AND job.installation_id=$2 AND job.type=$3
		  AND job.status='processing' AND job.attempts=$4
		  AND job.locked_by=$5 AND job.locked_until >= now()
		  AND job.actor_type='widget_user' AND job.actor_id=$6
		  AND job.resource_type=$7 AND job.resource_id=$8
		  AND installation.status='active' AND integration.status='active'
		FOR SHARE OF installation, integration`,
		job.ID, *job.InstallationID, LeadStatusRuleConfigureJobType, job.Attempts,
		*job.LockedBy, *job.ActorID, leadStatusRuleResourceType, *job.ResourceID,
	).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadStatusRuleResult{}, ErrExecutionNotAuthorized
	}
	if err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("authorize rule configuration: %w", err)
	}

	result := LeadStatusRuleResult{
		SourcePipelineID: command.SourcePipelineID, SourceStatusID: command.SourceStatusID,
		TargetPipelineID: command.TargetPipelineID, TargetStatusID: command.TargetStatusID,
		Enabled: command.Enabled,
	}
	if command.ExpectedRevision == 0 {
		result.Revision = 1
		err = tx.QueryRow(ctx, `
			INSERT INTO lead_status_workflow_rules (
				installation_id,source_pipeline_id,source_status_id,
				target_pipeline_id,target_status_id,enabled,revision
			) VALUES ($1,$2,$3,$4,$5,$6,1)
			ON CONFLICT (installation_id,source_pipeline_id,source_status_id) DO NOTHING
			RETURNING id`, *job.InstallationID, command.SourcePipelineID,
			command.SourceStatusID, command.TargetPipelineID, command.TargetStatusID,
			command.Enabled).Scan(&result.RuleID)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE lead_status_workflow_rules
			SET target_pipeline_id=$4,target_status_id=$5,enabled=$6,
				revision=revision+1,updated_at=now()
			WHERE installation_id=$1 AND source_pipeline_id=$2 AND source_status_id=$3
			  AND revision=$7
			RETURNING id,revision`, *job.InstallationID, command.SourcePipelineID,
			command.SourceStatusID, command.TargetPipelineID, command.TargetStatusID,
			command.Enabled, command.ExpectedRevision).Scan(&result.RuleID, &result.Revision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadStatusRuleResult{}, ErrRuleRevisionConflict
	}
	if err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("apply lead status rule CAS: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lead_status_workflow_rule_configurations (
			job_id,installation_id,rule_id,actor_user_id,source_pipeline_id,
			source_status_id,target_pipeline_id,target_status_id,enabled,revision
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		job.ID, *job.InstallationID, result.RuleID, userID, result.SourcePipelineID,
		result.SourceStatusID, result.TargetPipelineID, result.TargetStatusID,
		result.Enabled, result.Revision,
	); err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("record rule configuration receipt: %w", err)
	}
	metadata, _ := json.Marshal(map[string]any{
		"source_pipeline_id": result.SourcePipelineID, "source_status_id": result.SourceStatusID,
		"target_pipeline_id": result.TargetPipelineID, "target_status_id": result.TargetStatusID,
		"enabled": result.Enabled, "revision": result.Revision,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (
			installation_id,actor_type,actor_id,action,object_type,object_id,
			correlation_job_id,metadata
		) VALUES ($1,'widget_user',$2,$3,'lead_status_workflow_rule',$4,$5,$6)
		ON CONFLICT (correlation_job_id) DO NOTHING`, *job.InstallationID,
		strconv.FormatInt(userID, 10), LeadStatusRuleConfigureJobType,
		result.RuleID.String(), job.ID, metadata,
	); err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("audit rule configuration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LeadStatusRuleResult{}, fmt.Errorf("commit rule configuration: %w", err)
	}
	return result, nil
}

type rowScanner interface{ Scan(...any) error }

func scanRuleResult(row rowScanner) (LeadStatusRuleResult, error) {
	var result LeadStatusRuleResult
	err := row.Scan(&result.RuleID, &result.SourcePipelineID, &result.SourceStatusID,
		&result.TargetPipelineID, &result.TargetStatusID, &result.Enabled, &result.Revision)
	return result, err
}
