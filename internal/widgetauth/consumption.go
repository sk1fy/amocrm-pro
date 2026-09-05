package widgetauth

import (
	"errors"
	"net/http"
)

// ConsumptionMiddleware completes read-only authentication after verification
// and admission limits. Product actions instead consume the token atomically
// with idempotency and enqueue. Only use with a verified context principal.
func ConsumptionMiddleware(authenticator *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := PrincipalFromContext(r.Context())
			if !ok || authenticator == nil || principal.TokenID == "" ||
				!principal.TokenRetainUntil.After(authenticator.clock()) {
				unauthorized(w)
				return
			}
			if err := authenticator.repository.ConsumeToken(r.Context(), principal.UsedToken()); err != nil {
				authenticator.logAuthRejection(r, err)
				if errors.Is(err, ErrReplay) {
					unauthorized(w)
				} else {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
