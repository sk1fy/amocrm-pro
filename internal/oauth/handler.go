package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

const (
	maxIntegrationCode = 128
	maxReturnURL       = 1024
	maxOAuthState      = 256
	maxOAuthCode       = 2048
	maxReferer         = 256
)

type oauthFlow interface {
	Start(context.Context, string, string) (string, error)
	Callback(context.Context, string, string, string) (InstallationResult, error)
}

type Handler struct {
	flow    oauthFlow
	consume func(context.Context, string)
	logger  *slog.Logger
}

func NewHandler(service *Service, logger *slog.Logger) *Handler {
	return &Handler{
		flow: service,
		consume: func(ctx context.Context, state string) {
			_, _ = service.store.ConsumeState(ctx, strings.TrimSpace(state))
		},
		logger: logger,
	}
}

func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("integration_code")
	returnURL := r.URL.Query().Get("return_url")
	if len(code) > maxIntegrationCode || len(returnURL) > maxReturnURL {
		writeOAuthError(w, http.StatusBadRequest, "invalid_argument", "cannot start authorization", false)
		return
	}
	authorizeURL, err := h.flow.Start(r.Context(), code, returnURL)
	if err != nil {
		if !errors.Is(err, ErrIntegrationNotFound) {
			h.logger.Error("start OAuth", "error", err, "request_id", httpmiddleware.RequestIDFromContext(r.Context()))
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_argument", "cannot start authorization", false)
		return
	}
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := query.Get("state")
	code := query.Get("code")
	referer := query.Get("referer")
	denied := query.Get("error")
	if len(state) > maxOAuthState {
		writeOAuthError(w, http.StatusBadRequest, "invalid_argument", "authorization failed", false)
		return
	}
	if denied != "" {
		h.consume(r.Context(), state)
		writeOAuthError(w, http.StatusBadRequest, "invalid_argument", "authorization failed", false)
		return
	}
	if len(code) > maxOAuthCode || len(referer) > maxReferer {
		writeOAuthError(w, http.StatusBadRequest, "invalid_argument", "authorization failed", false)
		return
	}
	result, err := h.flow.Callback(r.Context(), state, code, referer)
	if err != nil {
		status := http.StatusBadGateway
		code := "unavailable"
		retryable := true
		if errors.Is(err, ErrInvalidState) {
			status = http.StatusBadRequest
			code = "invalid_argument"
			retryable = false
		}
		h.logger.Error("complete OAuth", "error", err, "request_id", httpmiddleware.RequestIDFromContext(r.Context()))
		writeOAuthError(w, status, code, "authorization failed", retryable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(result)
}

type oauthErrorBody struct {
	Error oauthErrorFields `json:"error"`
}

type oauthErrorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(oauthErrorBody{Error: oauthErrorFields{
		Code: code, Message: message, RequestID: w.Header().Get("X-Request-ID"), Retryable: retryable,
	}})
}
