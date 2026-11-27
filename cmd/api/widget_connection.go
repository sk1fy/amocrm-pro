package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

// A browser needs to read the authentication response before the account has
// a Core installation. The JWT issuer must still match the browser origin.
type amoCRMConnectionOrigins struct{}

func (amoCRMConnectionOrigins) IsActiveOrigin(_ context.Context, origin string) (bool, error) {
	base, err := amocrm.AccountBaseURL(origin)
	return err == nil && base.String() == origin, nil
}

func registerWidgetConnection(router chi.Router, store *corelifecycle.Store,
	authenticator *widgetauth.Authenticator, limiter *widgetlimit.Limiter,
) {
	route := apicontract.WidgetConnection
	cors := widgetcors.Middleware(amoCRMConnectionOrigins{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := widgetauth.PrincipalFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, err := store.AccessProofScope(r.Context(), p)
		switch {
		case errors.Is(err, corelifecycle.ErrAccessProofDenied):
			w.WriteHeader(http.StatusForbidden)
		case err != nil:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	protected := protectWidgetRoute(authenticator, limiter, cors, true, handler)
	router.Method(route.Method, route.Path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		protected.ServeHTTP(w, r)
	}))
	preflight := cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	router.MethodFunc(http.MethodOptions, route.Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
		if methods := r.Header.Values("Access-Control-Request-Method"); len(methods) != 1 || methods[0] != route.Method {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		preflight.ServeHTTP(w, r)
	})
}
