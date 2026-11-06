package adminread

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxAdminActorLength = 200

type actorKey struct{}

func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFrom(ctx context.Context) string {
	actor, _ := ctx.Value(actorKey{}).(string)
	return actor
}

// ActorFromContext is populated only by the shared admin listener authentication.
func ActorFromContext(ctx context.Context) string { return actorFrom(ctx) }

// Authenticate returns the admin bearer and X-Admin-Actor check for other admin
// listeners (the worker CRM read listener). It fails closed for an empty token
// and writes the same error envelope as the admin read listener.
func Authenticate(token string) func(http.Handler) http.Handler {
	return (&handler{token: token}).authenticate
}

func (h *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" || !timingSafeEqual(bearerOrEmpty(r), h.token) {
			writeError(w, r, errUnauthenticated())
			return
		}
		actor, err := parseAdminActor(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actor)))
	})
}

func parseAdminActor(r *http.Request) (string, error) {
	values := r.Header.Values("X-Admin-Actor")
	if len(values) != 1 {
		return "", errUnauthenticated()
	}
	actor := strings.TrimSpace(values[0])
	if actor == "" || strings.ContainsAny(actor, "\r\n") || utf8.RuneCountInString(actor) > maxAdminActorLength {
		return "", errUnauthenticated()
	}
	prefix, value, ok := strings.Cut(actor, ":")
	if !ok || strings.TrimSpace(prefix) == "" || strings.TrimSpace(value) == "" {
		return "", errUnauthenticated()
	}
	return actor, nil
}

func bearerOrEmpty(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return ""
	}
	return parts[1]
}

func timingSafeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
