package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type authFake struct {
	seen map[uuid.UUID]bool
	want Scope
}

func (f *authFake) AuthorizeRequest(_ context.Context, s Scope, n uuid.UUID, _ time.Time) error {
	if s != f.want || f.seen[n] {
		return ErrDenied
	}
	f.seen[n] = true
	return nil
}
func TestSignedRequestsRejectReplayTamperScopeAndHeaders(t *testing.T) {
	scope := Scope{"first", uuid.New(), uuid.New()}
	secret := strings.Repeat("a", 32)
	for _, kind := range []string{"valid", "replay", "body", "method", "query", "company", "install", "key", "duplicate", "expired", "future", "ungranted"} {
		t.Run(kind, func(t *testing.T) {
			fake := &authFake{map[uuid.UUID]bool{}, scope}
			a := Auth{Keys: map[string]string{"first": secret, "second": secret}, Store: fake}
			h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			r := httptest.NewRequest("POST", "/x?revision=1", strings.NewReader("{}"))
			Sign(r, scope, secret, []byte("{}"))
			want := 401
			switch kind {
			case "valid":
				want = 204
			case "replay":
				first := httptest.NewRecorder()
				h.ServeHTTP(first, r.Clone(context.Background()))
				if first.Code != 204 {
					t.Fatal(first.Code)
				}
			case "body":
				r.Body = http.NoBody
			case "method":
				r.Method = "GET"
			case "query":
				r.URL.RawQuery = "revision=2"
			case "company":
				r.Header.Set("X-Distribution-Company", uuid.NewString())
			case "install":
				r.Header.Set("X-Distribution-Installation", uuid.NewString())
			case "key":
				r.Header.Set("X-Distribution-Key-Id", "second")
			case "duplicate":
				r.Header.Add("X-Distribution-Key-Id", "first")
			case "expired":
				a.Clock = func() time.Time { return time.Now().Add(2 * time.Minute) }
				h = a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			case "future":
				a.Clock = func() time.Time { return time.Now().Add(-2 * time.Minute) }
				h = a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			case "ungranted":
				fake.want.CompanyID = uuid.New()
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if out.Code != want {
				t.Fatalf("got%d want%d", out.Code, want)
			}
		})
	}
}
func TestKeyConfigurationRejectsWeakOrInvalid(t *testing.T) {
	for _, raw := range []string{`{}`, `{"bad/key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"good":"short"}`} {
		if _, e := ParseKeys(raw); e == nil {
			t.Fatal("accepted invalid")
		}
	}
	if _, e := ParseKeys(`{"good":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`); e != nil {
		t.Fatal(e)
	}
}
func TestSourceFailureIsNotAuthenticationDenial(t *testing.T) {
	if errors.Is(ErrUnavailable, ErrDenied) {
		t.Fatal("wrong errors")
	}
}
