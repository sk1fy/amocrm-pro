package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func registerPlatformAccessProof(router chi.Router, store *corelifecycle.Store, signer *platformaccess.Signer,
	timeout time.Duration, protect func(bool, http.Handler) http.Handler, cors func(http.Handler) http.Handler,
) {
	issuer := func(w http.ResponseWriter, r *http.Request) {
		platformAccessProof(w.(*widgetProofAdmissionWriter).ResponseWriter, r, store, signer)
	}
	router.Method(http.MethodPost, platformaccess.IssuePath,
		adaptWidgetProofAdmission(timeout, protect(true, http.HandlerFunc(issuer))))
	preflight := cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	router.Method(http.MethodOptions, platformaccess.IssuePath,
		adaptWidgetProofAdmission(timeout, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
			method := r.Header.Values("Access-Control-Request-Method")
			if len(method) != 1 || method[0] != http.MethodPost {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			preflight.ServeHTTP(w, r)
		})))
}

func platformAccessProof(w http.ResponseWriter, r *http.Request, store *corelifecycle.Store, signer *platformaccess.Signer) {
	principal, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		platformaccess.WriteError(w, r.Context(), http.StatusUnauthorized, "unauthenticated", 0)
		return
	}
	scope, err := store.AccessProofScope(r.Context(), principal)
	switch {
	case errors.Is(err, corelifecycle.ErrAccessProofPolicy):
		platformaccess.WriteError(w, r.Context(), http.StatusServiceUnavailable, "policy_not_configured", 0)
	case errors.Is(err, corelifecycle.ErrAccessProofDenied):
		platformaccess.WriteError(w, r.Context(), http.StatusForbidden, "permission_denied", 0)
	case err != nil:
		platformaccess.WriteError(w, r.Context(), http.StatusServiceUnavailable, "unavailable", 1)
	default:
		issued, err := signer.Issue(scope, time.Now())
		if err != nil {
			platformaccess.WriteError(w, r.Context(), http.StatusServiceUnavailable, "policy_not_configured", 0)
			return
		}
		platformaccess.WriteJSON(w, http.StatusOK, issued)
	}
}

func adaptWidgetProofAdmission(timeout time.Duration, admission http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		response := &widgetProofAdmissionWriter{ResponseWriter: w, ctx: ctx}
		admission.ServeHTTP(response, r.WithContext(ctx))
	})
}

// Only admission passes through this writer. Its legacy error bodies are
// replaced with the P01 envelope, while its response headers remain intact.
type widgetProofAdmissionWriter struct {
	http.ResponseWriter
	ctx     context.Context
	discard bool
}

func (w *widgetProofAdmissionWriter) WriteHeader(status int) {
	if w.discard {
		return
	}
	if status < 400 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.discard = true
	switch status {
	case http.StatusUnauthorized:
		platformaccess.WriteError(w.ResponseWriter, w.ctx, status, "unauthenticated", 0)
	case http.StatusForbidden:
		platformaccess.WriteError(w.ResponseWriter, w.ctx, status, "permission_denied", 0)
	case http.StatusTooManyRequests:
		retry, _ := strconv.Atoi(w.Header().Get("Retry-After"))
		platformaccess.WriteError(w.ResponseWriter, w.ctx, status, "rate_limited", max(1, retry))
	default:
		platformaccess.WriteError(w.ResponseWriter, w.ctx, http.StatusServiceUnavailable, "unavailable", 1)
	}
}

func (w *widgetProofAdmissionWriter) Write(body []byte) (int, error) {
	if w.discard {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}
