package widgetapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type Handler struct {
	jobs    *jobs.Store
	actions *ActionStore
}

func NewHandler(jobStore *jobs.Store, actionStore *ActionStore) *Handler {
	return &Handler{jobs: jobStore, actions: actionStore}
}

func (h *Handler) Bootstrap(w http.ResponseWriter, r *http.Request) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installation_id": principal.InstallationID,
		"account_id":      principal.AccountID,
		"user_id":         principal.UserID,
		"client_uuid":     principal.ClientUUID,
	})
}

// Ping is an infrastructure-level asynchronous action. Product actions are
// deliberately added through the Worker registry after their contracts are
// accepted; this endpoint proves the secure widget -> job -> result path.
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		http.Error(w, "request body must be empty", http.StatusBadRequest)
		return
	}
	idempotencyValues := r.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 {
		http.Error(w, "invalid idempotency key", http.StatusBadRequest)
		return
	}
	result, err := h.actions.EnqueuePing(r.Context(), principal, idempotencyValues[0])
	switch {
	case errors.Is(err, ErrInvalidIdempotencyKey):
		http.Error(w, "invalid idempotency key", http.StatusBadRequest)
		return
	case errors.Is(err, widgetauth.ErrReplay), errors.Is(err, ErrInactiveTenant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrIdempotencyInProgress):
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
	case errors.Is(err, ErrInvalidIdempotencyKey), errors.Is(err, ErrInvalidLeadStatus):
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	case errors.Is(err, widgetauth.ErrReplay), errors.Is(err, ErrInactiveTenant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrIdempotencyInProgress):
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

func (h *Handler) JobStatus(w http.ResponseWriter, r *http.Request) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := h.jobs.GetForInstallationActor(
		r.Context(), jobID, principal.InstallationID,
		widgetActorType, strconv.FormatInt(principal.UserID, 10),
	)
	if errors.Is(err, jobs.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if job.Type != PingJobType && job.Type != LeadSetStatusJobType {
		http.NotFound(w, r)
		return
	}
	response := map[string]any{
		"job_id":       job.ID,
		"type":         job.Type,
		"status":       job.Status,
		"attempts":     job.Attempts,
		"max_attempts": job.MaxAttempts,
		"created_at":   job.CreatedAt,
		"updated_at":   job.UpdatedAt,
	}
	if len(job.Result) > 0 {
		result, err := publicJobResult(job)
		if err != nil {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		response["result"] = result
	}
	if job.FinishedAt != nil {
		response["finished_at"] = job.FinishedAt
	}
	if job.LastErrorCode != nil {
		response["error"] = map[string]string{"code": *job.LastErrorCode}
	}
	writeJSON(w, http.StatusOK, response)
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

func publicJobResult(job jobs.Job) (any, error) {
	switch job.Type {
	case PingJobType:
		var result struct {
			Pong bool `json:"pong"`
		}
		if err := json.Unmarshal(job.Result, &result); err != nil || !result.Pong {
			return nil, errors.New("invalid ping result")
		}
		return result, nil
	case LeadSetStatusJobType:
		var result LeadStatusResult
		if err := json.Unmarshal(job.Result, &result); err != nil ||
			result.LeadID <= 0 || result.PipelineID <= 0 || result.StatusID <= 0 || !result.Converged {
			return nil, errors.New("invalid lead status result")
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
