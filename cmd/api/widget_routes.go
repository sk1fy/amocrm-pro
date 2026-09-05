package main

import (
	"net/http"

	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

// All widget routes share one limiter. Read requests consume only after rate
// admission; product actions consume within their durable admission transaction.
func protectWidgetRoute(authenticator *widgetauth.Authenticator, limiter *widgetlimit.Limiter,
	cors func(http.Handler) http.Handler, consume bool, handler http.Handler,
) http.Handler {
	if consume {
		handler = widgetauth.ConsumptionMiddleware(authenticator)(handler)
	}
	return cors(widgetauth.VerificationMiddleware(authenticator)(
		widgetcors.BindPrincipalIssuer(limiter.Middleware(handler)),
	))
}
