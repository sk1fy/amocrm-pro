package colorleads

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"io"
	"net/http"
	"strconv"
)

type Repository interface {
	ReadSettings(context.Context, widgetauth.Principal, Fence) (SettingsDocument, error)
	WriteSettings(context.Context, widgetauth.Principal, Fence, json.RawMessage, int64) (SettingsDocument, error)
	ReadColors(context.Context, widgetauth.Principal, Fence, []int64) (ColorsDocument, error)
	WriteColors(context.Context, widgetauth.Principal, Fence, []Color, int64) (ColorsDocument, error)
}
type Handler struct{ store Repository }

func NewHandler(store Repository) *Handler { return &Handler{store: store} }

// Register on a router protected by widgetauth.VerificationMiddleware, origin
// restrictions and admission limits. This package intentionally does not wire
// cmd/api or authorize requests by account/installation fields in JSON.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/widgets/color-leads/settings", h.serve)
	r.Put("/v1/widgets/color-leads/settings", h.serve)
	r.Post("/v1/widgets/color-leads/colors/query", h.serve)
	r.Put("/v1/widgets/color-leads/colors", h.serve)
}
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		respondError(w, ErrUnauthorized)
		return
	}
	f := Fence{}
	var err error
	if len(r.Header.Values("X-Routing-Epoch")) != 1 || len(r.Header.Values("X-Installation-Generation")) != 1 {
		respondError(w, ErrRouteRejected)
		return
	}
	f.Epoch, err = strconv.ParseInt(r.Header.Get("X-Routing-Epoch"), 10, 64)
	if err != nil {
		respondError(w, ErrRouteRejected)
		return
	}
	f.Generation, err = strconv.ParseInt(r.Header.Get("X-Installation-Generation"), 10, 64)
	if err != nil || f.Epoch <= 0 || f.Generation <= 0 {
		respondError(w, ErrRouteRejected)
		return
	}
	if h.store == nil {
		respondError(w, ErrNotConfigured)
		return
	}
	var result any
	switch {
	case r.Method == http.MethodGet:
		result, err = h.store.ReadSettings(r.Context(), p, f)
	case r.Method == http.MethodPost:
		var body struct {
			LeadIDs []int64 `json:"lead_ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		result, err = h.store.ReadColors(r.Context(), p, f, body.LeadIDs)
	case r.URL.Path == "/v1/widgets/color-leads/settings":
		var body struct {
			Settings json.RawMessage `json:"settings"`
			Expected *int64          `json:"expected_revision"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Expected == nil {
			respondError(w, ErrInvalid)
			return
		}
		result, err = h.store.WriteSettings(r.Context(), p, f, body.Settings, *body.Expected)
	default:
		var body struct {
			Colors   []Color `json:"colors"`
			Expected *int64  `json:"expected_revision"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Expected == nil || body.Colors == nil {
			respondError(w, ErrInvalid)
			return
		}
		result, err = h.store.WriteColors(r.Context(), p, f, body.Colors, *body.Expected)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		respondError(w, ErrInvalid)
		return false
	}
	return true
}
func respondError(w http.ResponseWriter, err error) {
	code, status := "temporarily_unavailable", http.StatusServiceUnavailable
	for _, candidate := range []struct {
		err    error
		status int
	}{{ErrInvalid, 400}, {ErrUnauthorized, 401}, {ErrRouteRejected, 409}, {ErrRevisionConflict, 409}, {ErrNotConfigured, 503}} {
		if errors.Is(err, candidate.err) {
			code, status = candidate.err.Error(), candidate.status
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}
