package oauth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

type Handler struct {
	service *Service
	logger  *slog.Logger
}

func NewHandler(service *Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	authorizeURL, err := h.service.Start(
		r.Context(), r.URL.Query().Get("integration_code"), r.URL.Query().Get("return_url"),
	)
	if err != nil {
		status := http.StatusBadRequest
		if !errors.Is(err, ErrIntegrationNotFound) {
			h.logger.Error("start OAuth", "error", err, "request_id", httpmiddleware.RequestIDFromContext(r.Context()))
		}
		http.Error(w, "cannot start authorization", status)
		return
	}
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("error") != "" {
		_, _ = h.service.store.ConsumeState(r.Context(), r.URL.Query().Get("state"))
		http.Error(w, "authorization denied", http.StatusBadRequest)
		return
	}
	result, err := h.service.Callback(
		r.Context(),
		r.URL.Query().Get("state"),
		r.URL.Query().Get("code"),
		r.URL.Query().Get("referer"),
	)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrInvalidState) {
			status = http.StatusBadRequest
		}
		h.logger.Error("complete OAuth", "error", err, "request_id", httpmiddleware.RequestIDFromContext(r.Context()))
		http.Error(w, "authorization failed", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(result)
}
