package admincommand

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/adminread"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
)

// Register runs after adminread.Register, under the same Bearer/actor middleware.
func Register(router chi.Router, store *Store) {
	router.Post(apicontract.AdminCommands.Path, func(w http.ResponseWriter, r *http.Request) {
		actor := adminread.ActorFromContext(r.Context())
		if actor == "" {
			writeError(w, &Error{Code: "unauthenticated", Message: "authentication required", Status: 401})
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writeError(w, invalid("one idempotency key is required"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		req, err := decodeRequest(r.Body)
		if err != nil {
			writeError(w, err)
			return
		}
		defer clear(req.Payload)
		receipt, err := store.Execute(r.Context(), actor, keys[0], req)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if receipt.State == "pending" || receipt.State == "running" {
			status = http.StatusAccepted
		}
		writeJSON(w, status, receipt)
	})
	router.Get(apicontract.AdminCommand.Path, func(w http.ResponseWriter, r *http.Request) {
		if adminread.ActorFromContext(r.Context()) == "" {
			writeError(w, &Error{Code: "unauthenticated", Message: "authentication required", Status: 401})
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil || id == uuid.Nil {
			writeError(w, invalid("invalid receipt identifier"))
			return
		}
		receipt, err := store.Get(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
}

func writeError(w http.ResponseWriter, err error) {
	safe := classify(err)
	status := safe.Status
	if status == 0 {
		status = 500
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": safe.Code, "message": safe.Message, "request_id": w.Header().Get("X-Request-ID"), "retryable": false}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
