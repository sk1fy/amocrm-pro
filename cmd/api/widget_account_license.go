package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const widgetAccountLicensePath = "/api/v1/widget/account-license"

type accountLicenseReceiptKey struct{}

func registerWidgetAccountLicense(router chi.Router,
	scopeFor func(context.Context, widgetauth.Principal, time.Time) (wire.ActivationScope, error),
	report func(context.Context, wire.AccountLicenseRequest) error,
	protect func(bool, http.Handler) http.Handler, cors func(http.Handler) http.Handler,
) {
	handler := protect(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		badRequest := func() { platformaccess.WriteError(w, ctx, http.StatusBadRequest, "invalid_account_license", 0) }
		principal, ok := widgetauth.PrincipalFromContext(ctx)
		if !ok {
			platformaccess.WriteError(w, ctx, http.StatusUnauthorized, "unauthenticated", 0)
			return
		}
		contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if contentType != "application/json" || r.URL.RawQuery != "" {
			badRequest()
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxAccountLicenseHintsBody))
		if err != nil {
			badRequest()
			return
		}
		hints, err := wire.DecodeAccountLicenseHints(raw)
		if err != nil {
			badRequest()
			return
		}
		receivedAt := ctx.Value(accountLicenseReceiptKey{}).(time.Time)
		scope, err := scopeFor(ctx, principal, receivedAt)
		if errors.Is(err, corelifecycle.ErrAccessProofDenied) || errors.Is(err, corelifecycle.ErrAccessProofPolicy) {
			platformaccess.WriteError(w, ctx, http.StatusNotFound, "account_license_unavailable", 0)
			return
		}
		if err != nil || report(ctx, wire.AccountLicenseRequest{Scope: scope, ReceivedAt: receivedAt, Hints: hints}) != nil {
			platformaccess.WriteError(w, ctx, http.StatusServiceUnavailable, "account_license_unavailable", 2)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	router.Method(http.MethodPost, widgetAccountLicensePath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := context.WithValue(r.Context(), accountLicenseReceiptKey{}, time.Now().UTC())
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	preflight := cors(http.NotFoundHandler())
	router.Method(http.MethodOptions, widgetAccountLicensePath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Header.Values("Access-Control-Request-Method")
		if len(method) != 1 || method[0] != http.MethodPost {
			w.Header().Add("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		preflight.ServeHTTP(w, r)
	}))
}
