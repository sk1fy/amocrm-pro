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

// VerificationMiddleware validates a token but defers durable jti consumption
// to the downstream action transaction.
func VerificationMiddleware(authenticator *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return authenticator.VerificationMiddleware(next)
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

// VerificationMiddleware adds a cryptographically verified principal to the
// context without consuming its jti. It is only safe when the next handler
// consumes Principal.UsedToken atomically with its durable side effect.
func (a *Authenticator) VerificationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawToken, ok := bearerToken(request.Header.Get("Authorization"))
		if !ok {
			unauthorized(response)
			return
		}

		principal, err := a.Verify(request.Context(), rawToken)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) {
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
