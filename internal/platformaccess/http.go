package platformaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

type errorResponse struct {
	Error problem `json:"error"`
}

type problem struct {
	Code              string    `json:"code"`
	Message           string    `json:"message"`
	RequestID         uuid.UUID `json:"request_id"`
	Retryable         bool      `json:"retryable"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
}

func WriteError(w http.ResponseWriter, ctx context.Context, status int, code string, retryAfter int) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	WriteJSON(w, status, errorResponse{Error: problem{
		Code: code, Message: http.StatusText(status), RequestID: httpmiddleware.RequestIDFromContext(ctx),
		Retryable: retryAfter > 0, RetryAfterSeconds: retryAfter,
	}})
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
