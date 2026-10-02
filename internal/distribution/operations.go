package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"strconv"
	"time"
)

const AssignmentJobType = "distribution.assign_responsible"

var ErrIdempotencyConflict = errors.New("idempotency conflict")
var ErrOperationUnresolved = errors.New("previous operation unresolved")
var ErrStaleDecision = errors.New("decision expired or unavailable")

type AssignmentScope struct {
	CompanyID       uuid.UUID `json:"companyId"`
	InstallationID  uuid.UUID `json:"installationId"`
	IntegrationID   uuid.UUID `json:"integrationId"`
	AccountID       int64     `json:"accountId,string"`
	BindingID       uuid.UUID `json:"bindingId"`
	BindingRevision int64     `json:"bindingRevision"`
}

func (s AssignmentScope) Binding() Binding {
	return Binding{ID: s.BindingID, CompanyID: s.CompanyID, InstallationID: s.InstallationID, IntegrationID: s.IntegrationID, AccountID: s.AccountID, Revision: s.BindingRevision}
}

type Snapshot struct {
	LeadID            int64      `json:"leadId,string"`
	PipelineID        int64      `json:"pipelineId,string"`
	StatusID          int64      `json:"statusId,string"`
	ResponsibleUserID int64      `json:"responsibleUserId,string"`
	SourceUpdatedAt   *time.Time `json:"sourceUpdatedAt"`
	ObservedAt        time.Time  `json:"observedAt"`
}
type Actor struct {
	Kind         string     `json:"kind"`
	TeamOSUserID *uuid.UUID `json:"teamosUserId,omitempty"`
	CRMUserID    *int64     `json:"crmUserId,string,omitempty"`
	OperatorID   *uuid.UUID `json:"operatorId,omitempty"`
}
type AssignmentCommand struct {
	OperationID             uuid.UUID `json:"operationId"`
	EpisodeID               uuid.UUID `json:"episodeId"`
	DecisionID              uuid.UUID `json:"decisionId"`
	RuleID                  uuid.UUID `json:"ruleId"`
	GroupID                 uuid.UUID `json:"groupId"`
	RuleRevision            int64     `json:"ruleRevision"`
	AvailabilityRevision    int64     `json:"availabilityRevision"`
	ClaimRevision           int64     `json:"claimRevision"`
	DecisionKind            string    `json:"decisionKind"`
	TargetResponsibleUserID int64     `json:"targetResponsibleUserId,string"`
	Expected                Snapshot  `json:"expectedSnapshot"`
	Actor                   Actor     `json:"actor"`
	ValidUntil              time.Time `json:"validUntil"`
}
type Assignment struct {
	SchemaVersion    int               `json:"schemaVersion"`
	MessageID        uuid.UUID         `json:"messageId"`
	Scope            AssignmentScope   `json:"scope"`
	EventID          uuid.UUID         `json:"eventId"`
	SourceEventID    *string           `json:"sourceEventId"`
	SourceOccurredAt time.Time         `json:"sourceOccurredAt"`
	ReceivedAt       time.Time         `json:"receivedAt"`
	EmittedAt        time.Time         `json:"emittedAt"`
	CorrelationID    uuid.UUID         `json:"correlationId"`
	CausationID      uuid.UUID         `json:"causationId"`
	Command          AssignmentCommand `json:"command"`
}
type Receipt struct {
	OperationID   uuid.UUID `json:"operationId"`
	AcceptedAt    time.Time `json:"acceptedAt"`
	State         string    `json:"state"`
	ResultVersion int64     `json:"resultVersion"`
}
type Evidence struct {
	Kind            string    `json:"kind"`
	ObservedAt      time.Time `json:"observedAt"`
	GuardReleasable bool      `json:"guardReleasable"`
	Explanation     string    `json:"explanation"`
}
type WireError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	Terminal          bool   `json:"terminal"`
	RetryAfterSeconds *int   `json:"retryAfterSeconds"`
}
type Operation struct {
	LeadID                  int64           `json:"leadId,string"`
	OperationID             uuid.UUID       `json:"operationId"`
	Scope                   AssignmentScope `json:"scope"`
	EpisodeID               uuid.UUID       `json:"episodeId"`
	DecisionID              uuid.UUID       `json:"decisionId"`
	RuleID                  uuid.UUID       `json:"ruleId"`
	GroupID                 uuid.UUID       `json:"groupId"`
	RuleRevision            int64           `json:"ruleRevision"`
	AvailabilityRevision    int64           `json:"availabilityRevision"`
	ClaimRevision           int64           `json:"claimRevision"`
	EventID                 uuid.UUID       `json:"eventId"`
	CorrelationID           uuid.UUID       `json:"correlationId"`
	TargetResponsibleUserID int64           `json:"targetResponsibleUserId,string"`
	State                   string          `json:"state"`
	ExternalEffectState     string          `json:"externalEffectState"`
	Outcome                 *string         `json:"outcome"`
	ResultVersion           int64           `json:"resultVersion"`
	ConfirmedSnapshot       *Snapshot       `json:"confirmedSnapshot"`
	ResolutionEvidence      Evidence        `json:"resolutionEvidence"`
	ErrorCode               *string         `json:"-"`
	Error                   *WireError      `json:"error"`
	AcceptedAt              time.Time       `json:"acceptedAt"`
	UpdatedAt               time.Time       `json:"updatedAt"`
	CancelRequestedAt       *time.Time      `json:"cancelRequestedAt"`
	Assignment              Assignment      `json:"-"`
	JobID                   uuid.UUID       `json:"-"`
	FinishedAt              *time.Time      `json:"-"`
}

func validRevision(v int64) bool { return v > 0 && v <= 9007199254740991 }
func (a Assignment) Validate() error {
	c, s := a.Command, a.Scope
	if a.SchemaVersion != 1 || a.MessageID == uuid.Nil || a.EventID == uuid.Nil || a.CorrelationID == uuid.Nil || a.CausationID == uuid.Nil || a.SourceOccurredAt.IsZero() || a.ReceivedAt.IsZero() || a.EmittedAt.IsZero() || s.CompanyID == uuid.Nil || s.InstallationID == uuid.Nil || s.IntegrationID == uuid.Nil || s.BindingID == uuid.Nil || s.AccountID <= 0 || !validRevision(s.BindingRevision) || c.OperationID == uuid.Nil || c.EpisodeID == uuid.Nil || c.DecisionID == uuid.Nil || c.RuleID == uuid.Nil || c.GroupID == uuid.Nil || !validRevision(c.RuleRevision) || !validRevision(c.AvailabilityRevision) || !validRevision(c.ClaimRevision) || c.TargetResponsibleUserID <= 0 || c.Expected.LeadID <= 0 || c.Expected.PipelineID <= 0 || c.Expected.StatusID <= 0 || c.Expected.ResponsibleUserID <= 0 || c.Expected.ObservedAt.IsZero() || c.ValidUntil.IsZero() {
		return ErrDenied
	}
	if c.DecisionKind != "assign" && c.DecisionKind != "keep" || c.DecisionKind == "keep" && c.TargetResponsibleUserID != c.Expected.ResponsibleUserID {
		return ErrDenied
	}
	if c.Actor.Kind != "system" && c.Actor.Kind != "user" {
		return ErrDenied
	}
	if c.Actor.Kind == "system" && (c.Actor.TeamOSUserID != nil || c.Actor.CRMUserID != nil || c.Actor.OperatorID != nil) {
		return ErrDenied
	}
	if c.Actor.OperatorID != nil {
		return ErrDenied
	}
	if c.Actor.Kind == "user" && (c.Actor.TeamOSUserID == nil || *c.Actor.TeamOSUserID == uuid.Nil || c.Actor.CRMUserID == nil || *c.Actor.CRMUserID <= 0) {
		return ErrDenied
	}
	if a.SourceEventID != nil && len(*a.SourceEventID) > 255 {
		return ErrDenied
	}
	return nil
}
func (s *Store) Admit(ctx context.Context, identity Scope, key string, a Assignment) (Receipt, bool, error) {
	keyID, keyErr := uuid.Parse(key)
	if keyErr != nil || keyID == uuid.Nil || keyID.String() != key {
		return Receipt{}, false, ErrDenied
	}
	a.SourceOccurredAt = a.SourceOccurredAt.UTC()
	a.ReceivedAt = a.ReceivedAt.UTC()
	a.EmittedAt = a.EmittedAt.UTC()
	a.Command.ValidUntil = a.Command.ValidUntil.UTC()
	a.Command.Expected.ObservedAt = a.Command.Expected.ObservedAt.UTC()
	if a.Command.Expected.SourceUpdatedAt != nil {
		u := a.Command.Expected.SourceUpdatedAt.UTC()
		a.Command.Expected.SourceUpdatedAt = &u
	}

	if len(key) < 1 || len(key) > 200 || a.Validate() != nil || a.Scope.CompanyID != identity.CompanyID || a.Scope.InstallationID != identity.InstallationID {
		return Receipt{}, false, ErrDenied
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return Receipt{}, false, err
	}
	hash := sha256.Sum256(raw)
	kh := sha256.Sum256([]byte(key))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer tx.Rollback(ctx)
	// Lock receipt namespace first; same business request remains readable after
	// expiry/revocation, provided the authenticated immutable scope still matches.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, a.Scope.BindingID.String()+":"+key); err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1))`, a.Command.OperationID.String()); err != nil {
		return Receipt{}, false, err
	}
	var oldHash, oldReceipt []byte
	var oldID, oldCompany, oldInstallation uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id,request_hash,receipt,company_id,installation_id FROM distribution_operations WHERE binding_id=$1 AND key_hash=$2`, a.Scope.BindingID, kh[:]).Scan(&oldID, &oldHash, &oldReceipt, &oldCompany, &oldInstallation)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,request_hash,receipt,company_id,installation_id FROM distribution_operations WHERE id=$1`, a.Command.OperationID).Scan(&oldID, &oldHash, &oldReceipt, &oldCompany, &oldInstallation)
	}
	if err == nil {
		if oldCompany != identity.CompanyID || oldInstallation != identity.InstallationID {
			return Receipt{}, false, ErrNotFound
		}
		if !equalBytes(oldHash, hash[:]) || oldID != a.Command.OperationID {
			return Receipt{}, false, ErrIdempotencyConflict
		}
		var r Receipt
		if json.Unmarshal(oldReceipt, &r) != nil {
			return r, false, ErrUnavailable
		}
		return r, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, err
	}
	if !a.Command.ValidUntil.After(time.Now()) || a.Command.ValidUntil.After(time.Now().Add(15*time.Minute)) {
		return Receipt{}, false, ErrStaleDecision
	}
	if err = services.RequireEnabled(ctx, tx, a.Scope.InstallationID, services.LeadDistribution, true); err != nil {
		return Receipt{}, false, err
	}
	var marker int
	err = tx.QueryRow(ctx, `SELECT 1 FROM distribution_bindings WHERE id=$1 AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 AND state='active' FOR SHARE`, a.Scope.BindingID, a.Scope.CompanyID, a.Scope.InstallationID, a.Scope.IntegrationID, a.Scope.AccountID, a.Scope.BindingRevision).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, ErrDenied
	}
	if err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, strconv.FormatInt(a.Scope.AccountID, 10)+":"+strconv.FormatInt(a.Command.Expected.LeadID, 10)); err != nil {
		return Receipt{}, false, err
	}
	var guarded bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_lead_guards WHERE account_id=$1 AND lead_id=$2)`, a.Scope.AccountID, a.Command.Expected.LeadID).Scan(&guarded); err != nil {
		return Receipt{}, false, err
	}
	if guarded {
		return Receipt{}, false, ErrOperationUnresolved
	}
	job, err := jobs.NewStore(s.pool).EnqueueTx(ctx, tx, jobs.EnqueueParams{InstallationID: &a.Scope.InstallationID, Type: AssignmentJobType, ActorType: "integration", ActorID: a.Scope.InstallationID.String(), ResourceType: "lead", ResourceID: strconv.FormatInt(a.Command.Expected.LeadID, 10), Payload: map[string]any{"operationId": a.Command.OperationID}, MaxAttempts: 5})
	if err != nil {
		return Receipt{}, false, err
	}
	receipt := Receipt{a.Command.OperationID, time.Now().UTC(), "queued", 1}
	rr, _ := json.Marshal(receipt)
	_, err = tx.Exec(ctx, `INSERT INTO distribution_operations(id,binding_id,company_id,installation_id,integration_id,account_id,binding_revision,decision_id,lead_id,key_hash,request_hash,command,receipt,job_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, a.Command.OperationID, a.Scope.BindingID, a.Scope.CompanyID, a.Scope.InstallationID, a.Scope.IntegrationID, a.Scope.AccountID, a.Scope.BindingRevision, a.Command.DecisionID, a.Command.Expected.LeadID, kh[:], hash[:], raw, rr, job.ID)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return Receipt{}, false, ErrIdempotencyConflict
		}
		return Receipt{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO distribution_lead_guards(account_id,lead_id,operation_id) VALUES($1,$2,$3)`, a.Scope.AccountID, a.Command.Expected.LeadID, a.Command.OperationID); err != nil {
		return Receipt{}, false, err
	}
	op, err := readOperation(ctx, tx, a.Command.OperationID, false)
	if err != nil {
		return Receipt{}, false, err
	}
	if err = emit(ctx, tx, op); err != nil {
		return Receipt{}, false, err
	}
	return receipt, false, tx.Commit(ctx)
}
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type opReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readOperation(ctx context.Context, q opReader, id uuid.UUID, lock bool) (Operation, error) {
	var op Operation
	var raw []byte
	var snap []byte
	var evidence string
	sql := `SELECT id,command,job_id,state,external_effect_state,outcome,result_version,evidence,error_code,confirmed_snapshot,cancel_requested_at,created_at,updated_at,finished_at FROM distribution_operations WHERE id=$1`
	if lock {
		sql += " FOR UPDATE"
	}
	err := q.QueryRow(ctx, sql, id).Scan(&op.OperationID, &raw, &op.JobID, &op.State, &op.ExternalEffectState, &op.Outcome, &op.ResultVersion, &evidence, &op.ErrorCode, &snap, &op.CancelRequestedAt, &op.AcceptedAt, &op.UpdatedAt, &op.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, err
	}
	if err = json.Unmarshal(raw, &op.Assignment); err != nil {
		return op, err
	}
	if op.ErrorCode != nil && *op.ErrorCode != "cancelled_before_dispatch" && *op.ErrorCode != "dispatch_intent_saved" && *op.ErrorCode != "cancellation_requested" {
		code := wireErrorCode(*op.ErrorCode)
		op.Error = &WireError{code, "Операция требует проверки подключения, решения или внешнего результата.", op.FinishedAt == nil, op.FinishedAt != nil, nil}
	}
	a, c := op.Assignment, op.Assignment.Command
	op.LeadID = c.Expected.LeadID
	op.Scope = a.Scope
	op.EpisodeID = c.EpisodeID
	op.DecisionID = c.DecisionID
	op.RuleID = c.RuleID
	op.GroupID = c.GroupID
	op.RuleRevision = c.RuleRevision
	op.AvailabilityRevision = c.AvailabilityRevision
	op.ClaimRevision = c.ClaimRevision
	op.EventID = a.EventID
	op.CorrelationID = a.CorrelationID
	op.TargetResponsibleUserID = c.TargetResponsibleUserID
	op.ResolutionEvidence = Evidence{evidence, op.UpdatedAt, op.FinishedAt != nil, "Исход подтверждается сохранённым свидетельством; наблюдение само по себе не доказывает авторство."}
	if len(snap) > 0 {
		op.ConfirmedSnapshot = &Snapshot{}
		if err = json.Unmarshal(snap, op.ConfirmedSnapshot); err != nil {
			return op, err
		}
	}
	return op, nil
}
func emit(ctx context.Context, tx pgx.Tx, op Operation) error {
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	outbox, err := json.Marshal(ResultEnvelope{op.Assignment.SchemaVersion, uuid.New(), op.Scope, op.EventID, op.Assignment.SourceEventID, op.Assignment.SourceOccurredAt, op.Assignment.ReceivedAt, time.Now().UTC(), op.CorrelationID, op.Assignment.MessageID, op})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO distribution_operation_results(operation_id,result_version,payload) VALUES($1,$2,$3)`, op.OperationID, op.ResultVersion, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO distribution_result_outbox(operation_id,result_version,payload) VALUES($1,$2,$3)`, op.OperationID, op.ResultVersion, outbox)
	return err
}
func (s *Store) Operation(ctx context.Context, identity Scope, id uuid.UUID) (Operation, error) {
	op, err := readOperation(ctx, s.pool, id, false)
	if err != nil {
		return op, err
	}
	if op.Scope.CompanyID != identity.CompanyID || op.Scope.InstallationID != identity.InstallationID {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

type ResultEnvelope struct {
	SchemaVersion    int             `json:"schemaVersion"`
	MessageID        uuid.UUID       `json:"messageId"`
	Scope            AssignmentScope `json:"scope"`
	EventID          uuid.UUID       `json:"eventId"`
	SourceEventID    *string         `json:"sourceEventId"`
	SourceOccurredAt time.Time       `json:"sourceOccurredAt"`
	ReceivedAt       time.Time       `json:"receivedAt"`
	EmittedAt        time.Time       `json:"emittedAt"`
	CorrelationID    uuid.UUID       `json:"correlationId"`
	CausationID      uuid.UUID       `json:"causationId"`
	Result           Operation       `json:"result"`
}

func wireErrorCode(reason string) string {
	switch reason {
	case "capability_revoked", "permission_denied", "source_unavailable", "policy_unavailable", "mapping_required", "reauth_required", "resource_not_found", "rate_limited":
		return reason
	case "source_changed", "manual_change_after_response":
		return "revision_conflict"
	default:
		return "operation_unresolved"
	}
}
