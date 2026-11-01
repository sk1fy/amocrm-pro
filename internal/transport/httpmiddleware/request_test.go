package httpmiddleware

import (
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDRemainsStableAndRejectsZero(t *testing.T) {
	valid := uuid.New()
	for _, incoming := range []string{valid.String(), uuid.Nil.String(), "malformed", ""} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("X-Request-ID", incoming)
		response := httptest.NewRecorder()
		RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			first, second := RequestIDFromContext(r.Context()), RequestIDFromContext(r.Context())
			if first == uuid.Nil || first != second || w.Header().Get("X-Request-ID") != first.String() {
				t.Fatal("unstable or zero correlation")
			}
			if incoming == valid.String() && first != valid {
				t.Fatal("valid correlation changed")
			}
		})).ServeHTTP(response, request)
	}
}
