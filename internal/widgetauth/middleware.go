package widgetauth

import (
	"errors"
	"net/http"
	"strings"
)

// Middleware creates net/http middleware backed by authenticator.
func Middleware(authenticator *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return authenticator.Middleware(next)
	}
}

// Middleware authenticates a strict Authorization: Bearer token and adds its
// Principal to the request context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawToken, ok := bearerToken(request.Header.Get("Authorization"))
		if !ok {
			unauthorized(response)
			return
		}

		principal, err := a.Authenticate(request.Context(), rawToken)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrReplay) {
				unauthorized(response)
				return
			}
			http.Error(response, "internal server error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(response, request.WithContext(ContextWithPrincipal(request.Context(), principal)))
	})
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func unauthorized(response http.ResponseWriter) {
	response.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(response, "unauthorized", http.StatusUnauthorized)
}
