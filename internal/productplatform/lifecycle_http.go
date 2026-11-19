package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func LifecycleHandler(store *LifecycleStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wire.ActivationPath {
			store.activation(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if contentType != "application/json" {
			lifecycleHTTPError(w, http.StatusBadRequest, "invalid_event")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBody))
		if err != nil {
			lifecycleHTTPError(w, http.StatusBadRequest, "invalid_event")
			return
		}
		if _, err := wire.DecodeEvent(raw); err != nil {
			lifecycleHTTPError(w, http.StatusBadRequest, "invalid_event")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), lifecycleTimeout)
		defer cancel()
		receipt, err := store.Record(ctx, raw)
		switch {
		case errors.Is(err, ErrEventScope):
			lifecycleHTTPError(w, http.StatusNotFound, "scope_unmapped")
		case errors.Is(err, ErrEventConflict):
			lifecycleHTTPError(w, http.StatusConflict, "event_conflict")
		case err != nil:
			lifecycleHTTPError(w, http.StatusServiceUnavailable, "inbox_unavailable")
		default:
			writeLifecycleJSON(w, http.StatusAccepted, receipt)
		}
	})
}

func lifecycleHTTPError(w http.ResponseWriter, status int, code string) {
	writeLifecycleJSON(w, status, struct {
		Error map[string]any `json:"error"`
	}{Error: map[string]any{
		"code": code, "message": http.StatusText(status), "request_id": uuid.New(),
		"retryable": status == http.StatusServiceUnavailable,
	}})
}

func writeLifecycleJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
