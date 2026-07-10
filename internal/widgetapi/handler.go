package widgetapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type Handler struct {
	jobs *jobs.Store
}

func NewHandler(jobStore *jobs.Store) *Handler { return &Handler{jobs: jobStore} }

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
	job, err := h.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		InstallationID: &principal.InstallationID,
		Type:           "widget.ping",
		Priority:       50,
		MaxAttempts:    3,
		Payload: map[string]any{
			"account_id": principal.AccountID,
			"user_id":    principal.UserID,
		},
	})
	if err != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status})
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
