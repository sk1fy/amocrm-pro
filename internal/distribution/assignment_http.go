package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"io"
	"net/http"
	"net/url"
	"time"
)

func (h *Handler) operationError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, ErrIdempotencyConflict):
		fail(w, 409, "idempotency_conflict")
	case errors.Is(e, ErrOperationUnresolved):
		fail(w, 409, "operation_unresolved")
	case errors.Is(e, ErrStaleDecision):
		fail(w, 409, "invalid_transition")
	default:
		h.resultError(w, e)
	}
}
func (h *Handler) admitAssignment(w http.ResponseWriter, r *http.Request) {
	var a Assignment
	if decode(r, &a) != nil {
		fail(w, 400, "validation_failed")
		return
	}
	key := single(r, "Idempotency-Key")
	if id, e := uuid.Parse(key); e != nil || id == uuid.Nil || id.String() != key || a.Validate() != nil {
		fail(w, 400, "validation_failed")
		return
	}
	receipt, replayed, e := h.Store.Admit(r.Context(), scopeFrom(r), key, a)
	if e != nil {
		h.operationError(w, e)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	write(w, 202, receipt)
}
func operationID(r *http.Request) (uuid.UUID, error) {
	id, e := uuid.Parse(chi.URLParam(r, "operationId"))
	if e != nil || id == uuid.Nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}
func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	id, e := operationID(r)
	if e != nil {
		h.operationError(w, e)
		return
	}
	op, e := h.Store.Operation(r.Context(), scopeFrom(r), id)
	if e != nil {
		h.operationError(w, e)
		return
	}
	write(w, 200, op)
}

type operationAction struct {
	ExpectedResultVersion int64  `json:"expectedResultVersion"`
	Actor                 Actor  `json:"actor"`
	Reason                string `json:"reason"`
}

func readAction(r *http.Request) (operationAction, error) {
	var body operationAction
	e := decode(r, &body)
	if e != nil || !validRevision(body.ExpectedResultVersion) || len(body.Reason) == 0 || len(body.Reason) > 1000 {
		return body, ErrDenied
	}
	if body.Actor.Kind == "user" && (body.Actor.TeamOSUserID == nil || *body.Actor.TeamOSUserID == uuid.Nil || body.Actor.CRMUserID == nil || *body.Actor.CRMUserID <= 0 || body.Actor.OperatorID != nil) {
		return body, ErrDenied
	}
	if body.Actor.Kind != "system" && body.Actor.Kind != "user" {
		return body, ErrDenied
	}
	if body.Actor.Kind == "system" && (body.Actor.TeamOSUserID != nil || body.Actor.CRMUserID != nil || body.Actor.OperatorID != nil) {
		return body, ErrDenied
	}
	return body, nil
}
func (h *Handler) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id, e := operationID(r)
	if e != nil {
		h.operationError(w, e)
		return
	}
	body, e := readAction(r)
	if e != nil {
		fail(w, 400, "validation_failed")
		return
	}
	op, e := h.Store.CancelAction(r.Context(), scopeFrom(r), id, body, single(r, "Idempotency-Key"))
	if e != nil {
		h.operationError(w, e)
		return
	}
	write(w, 200, op)
}
func (h *Handler) reconcileOperation(w http.ResponseWriter, r *http.Request) {
	id, e := operationID(r)
	if e != nil {
		h.operationError(w, e)
		return
	}
	body, e := readAction(r)
	if e != nil {
		fail(w, 400, "validation_failed")
		return
	}
	cached, replayed, err := h.Store.ActionReplay(r.Context(), scopeFrom(r), id, "reconcile", single(r, "Idempotency-Key"), body)
	if err != nil {
		h.operationError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		write(w, 200, cached)
		return
	}

	op, e := h.Store.Operation(r.Context(), scopeFrom(r), id)
	if e != nil {
		h.operationError(w, e)
		return
	}
	if op.ResultVersion != body.ExpectedResultVersion {
		fail(w, 409, "revision_conflict")
		return
	}
	if op.FinishedAt != nil {
		result, err := h.Store.ReconcileAction(r.Context(), scopeFrom(r), id, body, single(r, "Idempotency-Key"), nil)
		if err != nil {
			h.operationError(w, err)
			return
		}
		write(w, 200, result)
		return
	}
	crm, ok := h.CRM.(AssignmentCRM)
	if !ok {
		fail(w, 503, "source_unavailable")
		return
	}
	worker := AssignmentWorker{Store: h.Store, CRM: crm}
	snapshot, e := worker.Observe(r.Context(), op)
	if e != nil {
		fail(w, 503, "source_unavailable")
		return
	}
	result, e := h.Store.ReconcileAction(r.Context(), scopeFrom(r), id, body, single(r, "Idempotency-Key"), &snapshot)
	if e != nil {
		h.operationError(w, e)
		return
	}
	write(w, 200, result)
}

// ValidateDecision asks the business owner for a fresh authorization bound to
// this exact operation/decision/fence. Current TeamOS RS-04 seam fails closed
// until the rule/episode/availability engine exists; it never fabricates grants.
func (h *Handler) ValidateDecision(ctx context.Context, op Operation, targetEmployee uuid.UUID, fence int) (DecisionAuthorization, error) {
	var result DecisionAuthorization
	if h.TeamOSURL == "" || h.Keys[h.TeamOSKeyID] == "" {
		return result, ErrUnavailable
	}
	c, s := op.Assignment.Command, op.Scope
	payload := map[string]any{"companyId": s.CompanyID, "installationId": s.InstallationID, "integrationId": s.IntegrationID, "accountId": fmtLead(s.AccountID), "bindingId": s.BindingID, "bindingRevision": s.BindingRevision, "operationId": op.OperationID, "decisionId": op.DecisionID, "episodeId": op.EpisodeID, "ruleId": op.RuleID, "groupId": op.GroupID, "ruleRevision": op.RuleRevision, "availabilityRevision": op.AvailabilityRevision, "claimRevision": op.ClaimRevision, "workerFence": fence, "targetEmployeeId": targetEmployee, "targetResponsibleUserId": fmtLead(c.TargetResponsibleUserID), "leadId": fmtLead(c.Expected.LeadID), "decisionKind": c.DecisionKind, "validUntil": c.ValidUntil, "actor": c.Actor}
	raw, e := json.Marshal(payload)
	if e != nil {
		return result, e
	}
	u, e := url.Parse(h.TeamOSURL + "/internal/v1/distribution/validate-decision")
	if e != nil || u.Scheme != "https" || u.User != nil {
		return result, ErrDenied
	}
	request, e := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(raw))
	if e != nil {
		return result, e
	}
	request.Header.Set("Content-Type", "application/json")
	Sign(request, Scope{h.TeamOSKeyID, s.CompanyID, s.InstallationID}, h.Keys[h.TeamOSKeyID], raw)
	client := h.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	safeClient := *client
	safeClient.Timeout = 3 * time.Second
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := safeClient.Do(request)
	if e != nil {
		return result, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, ErrUnavailable
	}
	rawResponse, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if readErr != nil || len(rawResponse) > MaxBody {
		return result, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(rawResponse))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil {
		return result, ErrUnavailable
	}

	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return result, ErrUnavailable
	}
	return result, nil
}
func (s *Store) AssignmentFailure(ctx context.Context, executor jobs.TxExecutor, job jobs.Job, failure jobs.Failure, status jobs.Status) error {
	if status != jobs.StatusDead && status != jobs.StatusFailed && status != jobs.StatusCancelled {
		return nil
	}
	tx, ok := executor.(pgx.Tx)
	if !ok {
		return ErrUnavailable
	}
	var id uuid.UUID
	e := tx.QueryRow(ctx, `SELECT id FROM distribution_operations WHERE job_id=$1`, job.ID).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	op, e := readOperation(ctx, tx, id, true)
	if e != nil {
		return e
	}
	if op.FinishedAt != nil {
		return nil
	}
	var attempted, accepted, notSent bool
	e = tx.QueryRow(ctx, `SELECT response_accepted,transport_outcome='not_sent' FROM distribution_operation_attempts WHERE operation_id=$1`, id).Scan(&accepted, &notSent)
	attempted = e == nil
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if !attempted || notSent {
		return transition(ctx, tx, op, "rejected", "no_attempt", "rejected", wireErrorCode(failure.Code), nil, true, "no_request_sent")
	}
	if accepted {
		return transition(ctx, tx, op, "confirming", "settled", "", wireErrorCode(failure.Code), nil, false, "observed_state_only")
	}
	return transition(ctx, tx, op, "outcome_unknown", "unknown", "", wireErrorCode(failure.Code), nil, false, "observed_state_only")
}
