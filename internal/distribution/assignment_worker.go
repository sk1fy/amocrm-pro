package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"strconv"
	"time"
)

type AssignmentCRM interface {
	GetLeadSnapshot(context.Context, uuid.UUID, int64) (amocrm.LeadState, error)
	PrepareLeadResponsible(context.Context, uuid.UUID) (amocrm.LeadResponsibleMutation, error)
	DistributionUser(context.Context, uuid.UUID, int64) (amocrm.DistributionUser, error)
}
type DecisionAuthorization struct {
	Allowed     bool      `json:"allowed"`
	OperationID uuid.UUID `json:"operationId"`
	DecisionID  uuid.UUID `json:"decisionId"`
	WorkerFence int       `json:"workerFence"`
	ValidUntil  time.Time `json:"validUntil"`
	Reason      string    `json:"reason"`
}
type DecisionValidator interface {
	ValidateDecision(context.Context, Operation, uuid.UUID, int) (DecisionAuthorization, error)
}
type AssignmentWorker struct {
	Store  *Store
	CRM    AssignmentCRM
	Policy DecisionValidator
}

func fmtLead(v int64) string { return strconv.FormatInt(v, 10) }
func targetMatches(a Assignment, s Snapshot) bool {
	c := a.Command
	return s.LeadID == c.Expected.LeadID && s.PipelineID == c.Expected.PipelineID && s.StatusID == c.Expected.StatusID && s.ResponsibleUserID == c.TargetResponsibleUserID
}
func sourceMatches(a Assignment, s Snapshot) bool {
	e := a.Command.Expected
	return s.LeadID == e.LeadID && s.PipelineID == e.PipelineID && s.StatusID == e.StatusID && s.ResponsibleUserID == e.ResponsibleUserID && (e.SourceUpdatedAt == nil || s.SourceUpdatedAt != nil && e.SourceUpdatedAt.Equal(*s.SourceUpdatedAt))
}
func snapshotOf(l amocrm.LeadState, started time.Time) Snapshot {
	updated := time.Unix(l.UpdatedAt, 0).UTC()
	return Snapshot{l.ID, l.PipelineID, l.StatusID, l.ResponsibleUserID, &updated, started.UTC()}
}
func (w *AssignmentWorker) Observe(ctx context.Context, op Operation) (Snapshot, error) {
	start, e := w.Store.ObservationStart(ctx)
	if e != nil {
		return Snapshot{}, e
	}
	l, e := w.CRM.GetLeadSnapshot(ctx, op.Scope.InstallationID, op.Assignment.Command.Expected.LeadID)
	if e != nil {
		return Snapshot{}, e
	}
	if l.ID != op.Assignment.Command.Expected.LeadID || l.ResponsibleUserID <= 0 || l.PipelineID <= 0 || l.StatusID <= 0 || l.UpdatedAt <= 0 {
		return Snapshot{}, amocrm.ErrIncompleteResponse
	}
	return snapshotOf(l, start), nil
}
func (w *AssignmentWorker) Handler(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
	var payload struct {
		OperationID uuid.UUID `json:"operationId"`
	}
	if json.Unmarshal(job.Payload, &payload) != nil || payload.OperationID == uuid.Nil {
		return nil, jobs.Permanent("invalid_payload", ErrDenied)
	}
	op, e := w.Store.CurrentJob(ctx, job, payload.OperationID)
	if e != nil {
		return nil, e
	}
	if op.FinishedAt != nil {
		return json.Marshal(op)
	}
	finish := func(state, effect, outcome, code string, snapshot *Snapshot, terminal bool, evidence string) (json.RawMessage, error) {
		result, err := w.Store.FinishJob(ctx, job, op.OperationID, state, effect, outcome, code, snapshot, terminal, evidence)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	attempt, e := w.Store.Attempt(ctx, op.OperationID)
	if e != nil {
		return nil, e
	}
	if attempt.Exists {
		if attempt.NotSent {
			return finish("rejected", "no_attempt", "rejected", "no_request_sent", nil, true, "no_request_sent")
		}
		observation, err := w.Observe(ctx, op)
		if err != nil {
			if attempt.Accepted {
				return finish("confirming", "settled", "", "source_unavailable", nil, false, "observed_state_only")
			}
			return finish("outcome_unknown", "unknown", "", "source_unavailable", nil, false, "observed_state_only")
		}
		if attempt.Accepted && freshAfterResponse(&observation, attempt.FinishedAt, attempt.AckUpdatedAt) {
			if targetMatches(op.Assignment, observation) {
				return finish("succeeded", "settled", "assigned", "", &observation, true, "response_and_observation")
			}
			return finish("conflict", "settled", "conflict", "manual_change_after_response", &observation, true, "response_and_observation")
		}
		return finish("outcome_unknown", "unknown", "", "external_outcome_unproven", &observation, false, "observed_state_only")
	}
	if op.CancelRequestedAt != nil {
		return finish("cancelled", "no_attempt", "cancelled", "", nil, true, "no_request_sent")
	}
	if !op.Assignment.Command.ValidUntil.After(time.Now()) {
		return finish("rejected", "no_attempt", "rejected", "decision_expired", nil, true, "no_request_sent")
	}
	if e = w.Store.Require(ctx, op.Scope.Binding()); e != nil {
		return finish("rejected", "no_attempt", "rejected", "capability_revoked", nil, true, "no_request_sent")
	}
	c := op.Assignment.Command
	var targetEmployee uuid.UUID
	if e = w.Store.pool.QueryRow(ctx, `SELECT employee_id FROM distribution_actor_mappings WHERE binding_id=$1 AND user_id=$2`, op.Scope.BindingID, c.TargetResponsibleUserID).Scan(&targetEmployee); e != nil {
		return finish("rejected", "no_attempt", "rejected", "mapping_required", nil, true, "no_request_sent")
	}
	prepared, e := w.CRM.PrepareLeadResponsible(ctx, op.Scope.InstallationID)
	if e != nil {
		return nil, retryAssignment("source_unavailable", e)
	}
	user, e := w.CRM.DistributionUser(ctx, op.Scope.InstallationID, c.TargetResponsibleUserID)
	if e != nil {
		return nil, retryAssignment("source_unavailable", e)
	}
	if user.ID != c.TargetResponsibleUserID || user.Rights.IsActive == nil || !*user.Rights.IsActive || user.Rights.IsFree {
		return finish("rejected", "no_attempt", "rejected", "recipient_unavailable", nil, true, "no_request_sent")
	}
	if c.Actor.Kind == "user" {
		if e = w.Store.Mapped(ctx, op.Scope.Binding(), *c.Actor.TeamOSUserID, *c.Actor.CRMUserID); e != nil {
			return finish("rejected", "no_attempt", "rejected", "permission_denied", nil, true, "no_request_sent")
		}
		policyCRM, ok := w.CRM.(CRM)
		if !ok {
			return nil, retryAssignment("policy_unavailable", ErrUnavailable)
		}
		allowed, e := CanViewLead(ctx, policyCRM, op.Scope.InstallationID, *c.Actor.CRMUserID, c.Expected.LeadID)
		if e != nil {
			return nil, retryAssignment("policy_unavailable", e)
		}
		if !allowed {
			return finish("rejected", "no_attempt", "rejected", "permission_denied", nil, true, "no_request_sent")
		}

	}

	observation, e := w.Observe(ctx, op)
	if e != nil {
		return nil, retryAssignment("source_unavailable", e)
	}
	if !sourceMatches(op.Assignment, observation) {
		return finish("conflict", "no_attempt", "source_changed", "source_changed", &observation, true, "no_request_sent")
	}
	if w.Policy == nil {
		return nil, retryAssignment("policy_unavailable", ErrUnavailable)
	}
	permission, e := w.Policy.ValidateDecision(ctx, op, targetEmployee, job.Attempts)
	if e != nil {
		return nil, retryAssignment("policy_unavailable", e)
	}
	if !permission.Allowed || permission.OperationID != op.OperationID || permission.DecisionID != op.DecisionID || permission.WorkerFence != job.Attempts || !permission.ValidUntil.After(time.Now()) || permission.ValidUntil.After(time.Now().Add(5*time.Second)) {
		return finish("rejected", "no_attempt", "rejected", "policy_unavailable", &observation, true, "no_request_sent")
	}
	if c.DecisionKind == "keep" || c.TargetResponsibleUserID == observation.ResponsibleUserID {
		// No-effect business turns require the same fresh grant after DB waits.
		outcome := "already_target"
		if c.DecisionKind == "keep" {
			outcome = "kept"
		}
		result, err := w.Store.FinishNoChange(ctx, job, op.OperationID, outcome, &observation, permission.ValidUntil)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	if e = w.Store.Dispatch(ctx, job, op, permission.ValidUntil); e != nil {
		return nil, e
	}
	// Recheck immediately after commit: a DB wait may exhaust the <=5s grant.
	// Once dispatch intent exists, failure is conservatively held until an exact
	// not-sent response is recorded. Never execute a second PATCH after takeover.
	if gateErr := w.Store.DispatchGate(ctx, job, op.OperationID, permission.ValidUntil); gateErr != nil {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e = w.Store.RecordResponse(recordCtx, op.OperationID, job.Attempts, false, true, 0, "authorization_expired_before_send", 0); e != nil {
			return nil, e
		}
		return finish("rejected", "no_attempt", "rejected", "policy_unavailable", nil, true, "no_request_sent")
	}
	result, assignErr := prepared.Assign(ctx, c.Expected.LeadID, c.TargetResponsibleUserID)
	accepted, notSent, status, code := assignErr == nil && result.Accepted, false, result.HTTPStatus, ""
	if assignErr != nil {
		code = "external_outcome_unproven"
		var de *amocrm.ResponsibleDispatchError
		if errors.As(assignErr, &de) {
			notSent = !de.Dispatched
			status = de.StatusCode
			switch status {
			case 401:
				code = "reauth_required"
			case 403:
				code = "permission_denied"
			case 404:
				code = "resource_not_found"
			case 429:
				code = "rate_limited"
			default:
				if status >= 500 {
					code = "source_unavailable"
				}
			}
		}
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e = w.Store.RecordResponse(recordCtx, op.OperationID, job.Attempts, accepted, notSent, status, code, result.UpdatedAt); e != nil {
		return nil, e
	}
	if notSent {
		return finish("rejected", "no_attempt", "rejected", "no_request_sent", nil, true, "no_request_sent")
	}
	observation, e = w.Observe(ctx, op)
	if e != nil {
		if accepted {
			return finish("confirming", "settled", "", "source_unavailable", nil, false, "observed_state_only")
		}
		return finish("outcome_unknown", "unknown", "", "source_unavailable", nil, false, "observed_state_only")
	}
	if !accepted {
		return finish("outcome_unknown", "unknown", "", code, &observation, false, "observed_state_only")
	}
	latestAttempt, err := w.Store.Attempt(ctx, op.OperationID)
	if err != nil {
		return nil, err
	}
	if !freshAfterResponse(&observation, latestAttempt.FinishedAt, latestAttempt.AckUpdatedAt) {
		return finish("confirming", "settled", "", "source_unavailable", &observation, false, "observed_state_only")
	}
	if targetMatches(op.Assignment, observation) {
		return finish("succeeded", "settled", "assigned", "", &observation, true, "response_and_observation")
	}
	return finish("conflict", "settled", "conflict", "manual_change_after_response", &observation, true, "response_and_observation")
}

func retryAssignment(code string, e error) error { return jobs.Retryable(code, 5*time.Second, e) }

// RegisterJobs keeps execution and exhaustion recovery in the same composition
// step: omitting the observer would strand safe no-attempt operations.
func (w *AssignmentWorker) RegisterJobs(handlers map[string]jobs.Handler, observers map[string]jobs.FailureObserver) {
	handlers[AssignmentJobType] = w.Handler
	observers[AssignmentJobType] = w.Store.AssignmentFailure
}
