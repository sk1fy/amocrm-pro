package widgetapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
	job, err := h.jobs.GetForInstallation(r.Context(), jobID, principal.InstallationID)
	if errors.Is(err, jobs.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	var owner struct {
		AccountID int64 `json:"account_id"`
		UserID    int64 `json:"user_id"`
	}
	if job.Type != "widget.ping" || json.Unmarshal(job.Payload, &owner) != nil ||
		owner.AccountID != principal.AccountID || owner.UserID != principal.UserID {
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
		response["result"] = job.Result
	}
	if job.FinishedAt != nil {
		response["finished_at"] = job.FinishedAt
	}
	if job.LastErrorCode != nil {
		response["error"] = map[string]string{"code": *job.LastErrorCode}
	}
	writeJSON(w, http.StatusOK, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
