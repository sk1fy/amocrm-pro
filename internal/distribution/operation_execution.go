package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"time"
)

type Attempt struct {
	Exists       bool
	Fence        int
	Accepted     bool
	Finished     bool
	NotSent      bool
	HTTPStatus   *int
	FinishedAt   *time.Time
	AckUpdatedAt int64
}

func (s *Store) Attempt(ctx context.Context, id uuid.UUID) (Attempt, error) {
	a := Attempt{}
	var end *time.Time
	var transport string
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT fence,response_accepted,response_finished_at,transport_outcome,http_status,response_evidence FROM distribution_operation_attempts WHERE operation_id=$1`, id).Scan(&a.Fence, &a.Accepted, &end, &transport, &a.HTTPStatus, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.Exists = true
	a.Finished = end != nil
	a.FinishedAt = end
	var evidence struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &evidence) != nil {
		return a, ErrUnavailable
	}
	a.AckUpdatedAt = evidence.UpdatedAt
	a.NotSent = transport == "not_sent"
	return a, nil
}
func lease(ctx context.Context, tx pgx.Tx, job jobs.Job, op Operation) error {
	if job.LockedBy == nil || job.InstallationID == nil || *job.InstallationID != op.Scope.InstallationID || job.ID != op.JobID || job.Type != AssignmentJobType {
		return jobs.ErrLeaseLost
	}
	var marker int
	err := tx.QueryRow(ctx, `SELECT 1 FROM jobs WHERE id=$1 AND installation_id=$2 AND type=$3 AND status='processing' AND attempts=$4 AND locked_by=$5 AND locked_until>clock_timestamp() AND actor_type='integration' AND actor_id=$6 AND resource_type='lead' AND resource_id=$7 FOR SHARE`, job.ID, op.Scope.InstallationID, AssignmentJobType, job.Attempts, *job.LockedBy, op.Scope.InstallationID.String(), fmtLead(op.Assignment.Command.Expected.LeadID)).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrLeaseLost
	}
	return err
}
func (s *Store) CurrentJob(ctx context.Context, job jobs.Job, id uuid.UUID) (Operation, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Operation{}, e
	}
	defer tx.Rollback(ctx)
	op, e := readOperation(ctx, tx, id, false)
	if e != nil {
		return op, e
	}
	if e = lease(ctx, tx, job, op); e != nil {
		return op, e
	}
	return op, tx.Commit(ctx)
}
func (s *Store) Dispatch(ctx context.Context, job jobs.Job, op Operation, permissionExpires time.Time) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	current, e := readOperation(ctx, tx, op.OperationID, false)
	if e != nil {
		return e
	}
	if e = lease(ctx, tx, job, current); e != nil {
		return e
	}
	current, e = readOperation(ctx, tx, op.OperationID, true)
	if e != nil {
		return e
	}
	if current.CancelRequestedAt != nil || current.FinishedAt != nil || !current.Assignment.Command.ValidUntil.After(time.Now()) || !permissionExpires.After(time.Now()) || permissionExpires.After(time.Now().Add(5*time.Second)) {
		return ErrStaleDecision
	}
	if e = requireAdmission(ctx, tx, current.Scope.InstallationID, true); e != nil {
		return e
	}
	if e = services.RequireEnabled(ctx, tx, current.Scope.InstallationID, services.LeadDistribution, true); e != nil {
		return e
	}
	var active bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_bindings WHERE id=$1 AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 AND state='active')`, current.Scope.BindingID, current.Scope.CompanyID, current.Scope.InstallationID, current.Scope.IntegrationID, current.Scope.AccountID, current.Scope.BindingRevision).Scan(&active); e != nil {
		return e
	}
	if !active {
		return ErrDenied
	}
	var holder uuid.UUID
	if e = tx.QueryRow(ctx, `SELECT operation_id FROM distribution_lead_guards WHERE account_id=$1 AND lead_id=$2 FOR UPDATE`, current.Scope.AccountID, current.Assignment.Command.Expected.LeadID).Scan(&holder); e != nil {
		return e
	}
	if holder != current.OperationID {
		return ErrOperationUnresolved
	}
	if e = lease(ctx, tx, job, current); e != nil {
		return e
	}
	if !permissionExpires.After(time.Now()) || !current.Assignment.Command.ValidUntil.After(time.Now()) || ctx.Err() != nil {
		return ErrStaleDecision
	}
	_, e = tx.Exec(ctx, `INSERT INTO distribution_operation_attempts(operation_id,job_id,fence,executor) VALUES($1,$2,$3,$4)`, current.OperationID, job.ID, job.Attempts, *job.LockedBy)
	if e != nil {
		return ErrOperationUnresolved
	}
	if e = transition(ctx, tx, current, "applying", "in_flight", "", "dispatch_intent_saved", nil, false, "observed_state_only"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) RecordResponse(ctx context.Context, id uuid.UUID, fence int, accepted, notSent bool, status int, code string, ackUpdatedAt int64) error {
	if accepted && ackUpdatedAt <= 0 {
		return ErrDenied
	}
	raw, _ := json.Marshal(map[string]any{"updatedAt": ackUpdatedAt})
	transport := "ambiguous"
	if notSent {
		transport = "not_sent"
	} else if accepted {
		transport = "response"
	}
	// A late worker may append its own dispatch evidence even after losing its
	// scheduler lease. It cannot change operation/result/guard state here.
	ct, e := s.pool.Exec(ctx, `UPDATE distribution_operation_attempts SET response_finished_at=now(),response_accepted=$3,http_status=NULLIF($4,0),transport_outcome=$5,error_code=NULLIF($6,''),response_evidence=$7 WHERE operation_id=$1 AND fence=$2 AND response_finished_at IS NULL`, id, fence, accepted, status, transport, code, raw)
	if e != nil {
		return e
	}
	if ct.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	return nil
}
func transition(ctx context.Context, tx pgx.Tx, op Operation, state, effect, outcome, code string, snapshot *Snapshot, terminal bool, evidence string) error {
	var raw any
	if snapshot != nil {
		encoded, e := json.Marshal(snapshot)
		if e != nil {
			return e
		}
		raw = encoded
	}
	var finished *time.Time
	if terminal {
		now := time.Now().UTC()
		finished = &now
	}
	_, e := tx.Exec(ctx, `UPDATE distribution_operations SET state=$2,external_effect_state=$3,outcome=NULLIF($4,''),error_code=NULLIF($5,''),confirmed_snapshot=$6,evidence=$7,result_version=result_version+1,finished_at=$8,updated_at=now() WHERE id=$1`, op.OperationID, state, effect, outcome, code, raw, evidence, finished)
	if e != nil {
		return e
	}
	if snapshot != nil {
		if _, e = tx.Exec(ctx, `INSERT INTO distribution_operation_observations(operation_id,snapshot) VALUES($1,$2)`, op.OperationID, raw); e != nil {
			return e
		}
	}
	if terminal {
		if _, e = tx.Exec(ctx, `DELETE FROM distribution_lead_guards WHERE account_id=$1 AND lead_id=$2 AND operation_id=$3`, op.Scope.AccountID, op.Assignment.Command.Expected.LeadID, op.OperationID); e != nil {
			return e
		}
	}
	current, e := readOperation(ctx, tx, op.OperationID, false)
	if e != nil {
		return e
	}
	return emit(ctx, tx, current)
}
func (s *Store) FinishJob(ctx context.Context, job jobs.Job, id uuid.UUID, state, effect, outcome, code string, snapshot *Snapshot, terminal bool, evidence string) (Operation, error) {
	return s.finishJob(ctx, job, id, state, effect, outcome, code, snapshot, terminal, evidence, nil)
}

// FinishNoChange settles a confirmed business turn without a CRM effect. The
// short TeamOS grant must still be valid after waiting for database locks; an
// expired grant cannot advance TeamOS's round-robin cursor through a result.
func (s *Store) FinishNoChange(ctx context.Context, job jobs.Job, id uuid.UUID, outcome string, snapshot *Snapshot, permissionExpires time.Time) (Operation, error) {
	return s.finishJob(ctx, job, id, "no_change", "no_attempt", outcome, "", snapshot, true, "no_request_sent", &permissionExpires)
}

func (s *Store) finishJob(ctx context.Context, job jobs.Job, id uuid.UUID, state, effect, outcome, code string, snapshot *Snapshot, terminal bool, evidence string, permissionExpires *time.Time) (Operation, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Operation{}, e
	}
	defer tx.Rollback(ctx)
	op, e := readOperation(ctx, tx, id, false)
	if e != nil {
		return op, e
	}
	if e = lease(ctx, tx, job, op); e != nil {
		return op, e
	}
	op, e = readOperation(ctx, tx, id, true)
	if e != nil {
		return op, e
	}
	if e = lease(ctx, tx, job, op); e != nil {
		return op, e
	}
	if op.FinishedAt != nil {
		return op, tx.Commit(ctx)
	}
	if permissionExpires != nil {
		if e = requireAdmission(ctx, tx, op.Scope.InstallationID, true); e != nil {
			return op, e
		}
		if e = services.RequireEnabled(ctx, tx, op.Scope.InstallationID, services.LeadDistribution, true); e != nil {
			return op, e
		}
		var active bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_bindings WHERE id=$1 AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 AND state='active')`, op.Scope.BindingID, op.Scope.CompanyID, op.Scope.InstallationID, op.Scope.IntegrationID, op.Scope.AccountID, op.Scope.BindingRevision).Scan(&active); e != nil {
			return op, e
		}
		if !active {
			return op, ErrDenied
		}
		var holder uuid.UUID
		if e = tx.QueryRow(ctx, `SELECT operation_id FROM distribution_lead_guards WHERE account_id=$1 AND lead_id=$2 FOR UPDATE`, op.Scope.AccountID, op.Assignment.Command.Expected.LeadID).Scan(&holder); e != nil {
			return op, e
		}
		if holder != op.OperationID {
			return op, ErrOperationUnresolved
		}
		if e = lease(ctx, tx, job, op); e != nil {
			return op, e
		}
		now := time.Now()
		if op.CancelRequestedAt != nil || !op.Assignment.Command.ValidUntil.After(now) || !permissionExpires.After(now) || permissionExpires.After(now.Add(5*time.Second)) || ctx.Err() != nil {
			return op, ErrStaleDecision
		}
	}
	if terminal {
		if e = terminalProof(ctx, tx, id, state, evidence, snapshot); e != nil {
			return op, e
		}
	}
	if e = transition(ctx, tx, op, state, effect, outcome, code, snapshot, terminal, evidence); e != nil {
		return op, e
	}
	result, e := readOperation(ctx, tx, id, false)
	if e != nil {
		return op, e
	}
	return result, tx.Commit(ctx)
}
func (s *Store) Cancel(ctx context.Context, identity Scope, id uuid.UUID, version int64) (Operation, error) {
	return s.cancel(ctx, identity, id, version, nil)
}
func (s *Store) CancelAction(ctx context.Context, identity Scope, id uuid.UUID, body operationAction, key string) (Operation, error) {
	info, e := newActionInfo("cancel", key, body, id)
	if e != nil {
		return Operation{}, e
	}
	return s.cancel(ctx, identity, id, body.ExpectedResultVersion, &info)
}
func (s *Store) cancel(ctx context.Context, identity Scope, id uuid.UUID, version int64, info *actionInfo) (Operation, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Operation{}, e
	}
	defer tx.Rollback(ctx)
	op, e := readOperation(ctx, tx, id, true)
	if e != nil {
		return op, e
	}
	if op.Scope.CompanyID != identity.CompanyID || op.Scope.InstallationID != identity.InstallationID {
		return Operation{}, ErrNotFound
	}
	if info != nil {
		cached, found, err := readActionReceipt(ctx, tx, id, *info)
		if err != nil {
			return op, err
		}
		if found {
			return cached, tx.Commit(ctx)
		}
	}
	if op.ResultVersion != version {
		return op, ErrConflict
	}
	if op.FinishedAt != nil {
		if info != nil {
			if err := saveActionReceipt(ctx, tx, op, identity, *info); err != nil {
				return op, err
			}
		}
		return op, tx.Commit(ctx)
	}
	if _, e = tx.Exec(ctx, `UPDATE distribution_operations SET cancel_requested_at=coalesce(cancel_requested_at,now()) WHERE id=$1`, id); e != nil {
		return op, e
	}
	var attempted bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_operation_attempts WHERE operation_id=$1)`, id).Scan(&attempted); e != nil {
		return op, e
	}
	if !attempted {
		e = transition(ctx, tx, op, "cancelled", "no_attempt", "cancelled", "cancelled_before_dispatch", nil, true, "no_request_sent")
	} else {
		e = transition(ctx, tx, op, op.State, op.ExternalEffectState, "", "cancellation_requested", op.ConfirmedSnapshot, false, op.ResolutionEvidence.Kind)
	}
	if e != nil {
		return op, e
	}
	op, e = readOperation(ctx, tx, id, false)
	if e != nil {
		return op, e
	}
	if info != nil {
		if err := saveActionReceipt(ctx, tx, op, identity, *info); err != nil {
			return op, err
		}
	}
	return op, tx.Commit(ctx)
}
func (s *Store) ReconcileObservation(ctx context.Context, identity Scope, id uuid.UUID, version int64, snapshot *Snapshot) (Operation, error) {
	return s.reconcile(ctx, identity, id, version, snapshot, nil)
}
func (s *Store) ReconcileAction(ctx context.Context, identity Scope, id uuid.UUID, body operationAction, key string, snapshot *Snapshot) (Operation, error) {
	info, e := newActionInfo("reconcile", key, body, id)
	if e != nil {
		return Operation{}, e
	}
	return s.reconcile(ctx, identity, id, body.ExpectedResultVersion, snapshot, &info)
}
func (s *Store) reconcile(ctx context.Context, identity Scope, id uuid.UUID, version int64, snapshot *Snapshot, info *actionInfo) (Operation, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Operation{}, e
	}
	defer tx.Rollback(ctx)
	op, e := readOperation(ctx, tx, id, true)
	if e != nil {
		return op, e
	}
	if op.Scope.CompanyID != identity.CompanyID || op.Scope.InstallationID != identity.InstallationID {
		return Operation{}, ErrNotFound
	}
	if info != nil {
		cached, found, err := readActionReceipt(ctx, tx, id, *info)
		if err != nil {
			return op, err
		}
		if found {
			return cached, tx.Commit(ctx)
		}
	}
	if info != nil {
		cached, found, err := readActionReceipt(ctx, tx, id, *info)
		if err != nil {
			return op, err
		}
		if found {
			return cached, tx.Commit(ctx)
		}
	}
	if op.ResultVersion != version {
		return op, ErrConflict
	}
	if op.FinishedAt != nil {
		if info != nil {
			if err := saveActionReceipt(ctx, tx, op, identity, *info); err != nil {
				return op, err
			}
		}
		return op, tx.Commit(ctx)
	}
	var accepted bool
	var notSent bool
	var finished *time.Time
	var proof []byte
	e = tx.QueryRow(ctx, `SELECT response_accepted,transport_outcome='not_sent',response_finished_at,response_evidence FROM distribution_operation_attempts WHERE operation_id=$1`, id).Scan(&accepted, &notSent, &finished, &proof)
	if errors.Is(e, pgx.ErrNoRows) {
		return op, ErrConflict
	}
	if e != nil {
		return op, e
	}
	state, effect, outcome, code, evidence, terminal := "outcome_unknown", "unknown", "", "external_outcome_unproven", "observed_state_only", false
	if notSent {
		state, effect, outcome, code, evidence, terminal = "rejected", "no_attempt", "rejected", "no_request_sent", "no_request_sent", true
	}
	var ack struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	if len(proof) > 0 && json.Unmarshal(proof, &ack) != nil {
		return op, ErrUnavailable
	}
	if accepted && snapshot != nil && freshAfterResponse(snapshot, finished, ack.UpdatedAt) {
		state, effect, outcome, code, evidence, terminal = "succeeded", "settled", "assigned", "", "response_and_observation", true
		if !targetMatches(op.Assignment, *snapshot) {
			state, outcome, code = "conflict", "conflict", "manual_change_after_response"
		}
	}
	if e = transition(ctx, tx, op, state, effect, outcome, code, snapshot, terminal, evidence); e != nil {
		return op, e
	}
	op, e = readOperation(ctx, tx, id, false)
	if e != nil {
		return op, e
	}
	if info != nil {
		if err := saveActionReceipt(ctx, tx, op, identity, *info); err != nil {
			return op, err
		}
	}
	return op, tx.Commit(ctx)
}

func freshAfterResponse(snapshot *Snapshot, finished *time.Time, updatedAt int64) bool {
	return snapshot != nil && finished != nil && updatedAt > 0 && !snapshot.ObservedAt.Before(*finished) && snapshot.SourceUpdatedAt != nil && snapshot.SourceUpdatedAt.Unix() >= updatedAt
}
func terminalProof(ctx context.Context, tx pgx.Tx, id uuid.UUID, state, evidence string, snapshot *Snapshot) error {
	var accepted, notSent bool
	var finished *time.Time
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT response_accepted,transport_outcome='not_sent',response_finished_at,response_evidence FROM distribution_operation_attempts WHERE operation_id=$1`, id).Scan(&accepted, &notSent, &finished, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		if evidence != "no_request_sent" {
			return ErrDenied
		}
		return nil
	}
	if err != nil {
		return err
	}
	if notSent && evidence == "no_request_sent" && (state == "rejected" || state == "cancelled") {
		return nil
	}
	var ack struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	if json.Unmarshal(raw, &ack) != nil {
		return ErrOperationUnresolved
	}
	if accepted && (state == "succeeded" || state == "conflict") && evidence == "response_and_observation" && freshAfterResponse(snapshot, finished, ack.UpdatedAt) {
		return nil
	}
	return ErrOperationUnresolved
}
func (s *Store) ObservationStart(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t)
	return t, err
}

func (s *Store) DispatchGate(ctx context.Context, job jobs.Job, id uuid.UUID, permissionExpires time.Time) error {
	if job.InstallationID == nil {
		return ErrDenied
	}
	if e := requireAdmission(ctx, s.pool, *job.InstallationID, false); e != nil {
		return e
	}
	if !permissionExpires.After(time.Now()) || ctx.Err() != nil {
		return ErrStaleDecision
	}
	op, e := s.CurrentJob(ctx, job, id)
	if e != nil {
		return e
	}
	if op.FinishedAt != nil || op.CancelRequestedAt != nil || !op.Assignment.Command.ValidUntil.After(time.Now()) {
		return ErrStaleDecision
	}
	if e = s.Require(ctx, op.Scope.Binding()); e != nil {
		return e
	}
	if !permissionExpires.After(time.Now()) || !op.Assignment.Command.ValidUntil.After(time.Now()) || ctx.Err() != nil {
		return ErrStaleDecision
	}
	return nil
}
