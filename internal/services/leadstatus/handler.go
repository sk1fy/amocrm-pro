package leadstatus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type Handler struct{ actions *ActionStore }

func NewHandler(actions *ActionStore) *Handler { return &Handler{actions: actions} }

func RegisterResults(handler *widgetapi.Handler) {
	handler.RegisterResult(LeadSetStatusJobType, func(raw json.RawMessage) (any, error) {
		return publicJobResult(jobs.Job{Type: LeadSetStatusJobType, Result: raw})
	})
	handler.RegisterResult(LeadStatusRuleConfigureJobType, func(raw json.RawMessage) (any, error) {
		return publicJobResult(jobs.Job{Type: LeadStatusRuleConfigureJobType, Result: raw})
	})
}
func (h *Handler) LeadSetStatus(w http.ResponseWriter, r *http.Request) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	command, err := decodeLeadStatusCommand(w, r)
	if err != nil {
		http.Error(w, "invalid lead status command", http.StatusBadRequest)
		return
	}
	idempotencyValues := r.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 {
		http.Error(w, "invalid idempotency key", http.StatusBadRequest)
		return
	}
	result, err := h.actions.EnqueueLeadSetStatus(
		r.Context(), principal, idempotencyValues[0], command,
	)
	switch {
	case errors.Is(err, services.ErrNotEnabled):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "service_not_enabled"}})
		return
	case errors.Is(err, widgetapi.ErrInvalidIdempotencyKey), errors.Is(err, ErrInvalidLeadStatus):
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	case errors.Is(err, widgetauth.ErrReplay), errors.Is(err, widgetapi.ErrInactiveTenant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case errors.Is(err, widgetapi.ErrIdempotencyConflict), errors.Is(err, widgetapi.ErrIdempotencyInProgress):
		http.Error(w, "idempotency conflict", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *Handler) ConfigureLeadStatusRule(w http.ResponseWriter, r *http.Request) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	command, err := decodeLeadStatusRuleCommand(w, r)
	if err != nil {
		http.Error(w, "invalid lead status rule command", http.StatusBadRequest)
		return
	}
	idempotencyValues := r.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 {
		http.Error(w, "invalid idempotency key", http.StatusBadRequest)
		return
	}
	result, err := h.actions.EnqueueLeadStatusRuleConfigure(
		r.Context(), principal, idempotencyValues[0], command,
	)
	switch {
	case errors.Is(err, services.ErrNotEnabled):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "service_not_enabled"}})
		return
	case errors.Is(err, widgetapi.ErrInvalidIdempotencyKey), errors.Is(err, ErrInvalidLeadStatusRule):
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	case errors.Is(err, widgetauth.ErrReplay), errors.Is(err, widgetapi.ErrInactiveTenant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case errors.Is(err, widgetapi.ErrIdempotencyConflict), errors.Is(err, widgetapi.ErrIdempotencyInProgress):
		http.Error(w, "idempotency conflict", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, result)
}

func decodeLeadStatusCommand(w http.ResponseWriter, r *http.Request) (LeadStatusCommand, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return LeadStatusCommand{}, ErrInvalidLeadStatus
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || len(body) == 0 {
		return LeadStatusCommand{}, ErrInvalidLeadStatus
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var command LeadStatusCommand
	if err := decoder.Decode(&command); err != nil {
		return LeadStatusCommand{}, ErrInvalidLeadStatus
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return LeadStatusCommand{}, ErrInvalidLeadStatus
	}
	if command.LeadID <= 0 || command.PipelineID <= 0 || command.StatusID <= 0 {
		return LeadStatusCommand{}, ErrInvalidLeadStatus
	}
	return command, nil
}

func decodeLeadStatusRuleCommand(w http.ResponseWriter, r *http.Request) (LeadStatusRuleCommand, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return LeadStatusRuleCommand{}, ErrInvalidLeadStatusRule
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil || len(body) == 0 {
		return LeadStatusRuleCommand{}, ErrInvalidLeadStatusRule
	}
	var input struct {
		SourcePipelineID *int64 `json:"source_pipeline_id"`
		SourceStatusID   *int64 `json:"source_status_id"`
		TargetPipelineID *int64 `json:"target_pipeline_id"`
		TargetStatusID   *int64 `json:"target_status_id"`
		Enabled          *bool  `json:"enabled"`
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return LeadStatusRuleCommand{}, ErrInvalidLeadStatusRule
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) ||
		input.SourcePipelineID == nil || input.SourceStatusID == nil ||
		input.TargetPipelineID == nil || input.TargetStatusID == nil ||
		input.Enabled == nil || input.ExpectedRevision == nil {
		return LeadStatusRuleCommand{}, ErrInvalidLeadStatusRule
	}
	command := LeadStatusRuleCommand{
		SourcePipelineID: *input.SourcePipelineID, SourceStatusID: *input.SourceStatusID,
		TargetPipelineID: *input.TargetPipelineID, TargetStatusID: *input.TargetStatusID,
		Enabled: *input.Enabled, ExpectedRevision: *input.ExpectedRevision,
	}
	if !validLeadStatusRuleCommand(command) {
		return LeadStatusRuleCommand{}, ErrInvalidLeadStatusRule
	}
	return command, nil
}

func publicJobResult(job jobs.Job) (any, error) {
	switch job.Type {
	case LeadSetStatusJobType:
		var result LeadStatusResult
		if err := json.Unmarshal(job.Result, &result); err != nil ||
			result.LeadID <= 0 || result.PipelineID <= 0 || result.StatusID <= 0 || !result.Converged {
			return nil, errors.New("invalid lead status result")
		}
		return result, nil
	case LeadStatusRuleConfigureJobType:
		var result LeadStatusRuleResult
		if err := json.Unmarshal(job.Result, &result); err != nil ||
			result.RuleID == uuid.Nil || result.SourcePipelineID <= 0 ||
			result.SourceStatusID <= 0 || result.TargetPipelineID <= 0 ||
			result.TargetStatusID <= 0 || result.Revision <= 0 {
			return nil, errors.New("invalid lead status rule result")
		}
		return result, nil
	default:
		return nil, jobs.ErrNotFound
	}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
