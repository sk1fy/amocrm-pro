package widgetauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestMiddlewareAddsVerifiedPrincipalToContext(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
	called := false
	handler := fixture.authenticator.Middleware(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		called = true
		principal, ok := PrincipalFromContext(request.Context())
		if !ok || principal.TokenID != testTokenID || principal.AccountID != 12345678 {
			t.Fatalf("PrincipalFromContext() = %+v/%v", principal, ok)
		}
		response.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/widget/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer "+rawToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if !called {
		t.Fatal("next handler was not called")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if fixture.repository.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", fixture.repository.consumeCalls)
	}
}

func TestMiddlewareRejectsMissingInvalidAndReplayedTokens(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		header       string
		configure    func(*authFixture)
		makeToken    bool
		wantConsumed int
	}{
		"missing":      {},
		"wrong scheme": {header: "Basic credentials"},
		"malformed":    {header: "Bearer not-a-jwt"},
		"replay": {
			makeToken: true,
			configure: func(fixture *authFixture) {
				fixture.repository.consumeError = ErrReplay
			},
			wantConsumed: 1,
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t)
			if test.configure != nil {
				test.configure(&fixture)
			}
			header := test.header
			if test.makeToken {
				header = "Bearer " + signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
			}
			called := false
			handler := Middleware(fixture.authenticator)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			request := httptest.NewRequest(http.MethodGet, "/widget/bootstrap", nil)
			request.Header.Set("Authorization", header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if called {
				t.Fatal("next handler was called")
			}
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("response status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			if fixture.repository.consumeCalls != test.wantConsumed {
				t.Fatalf("consume calls = %d, want %d", fixture.repository.consumeCalls, test.wantConsumed)
			}
		})
	}
}

func TestMiddlewareReturnsInternalServerErrorForRepositoryFailure(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	fixture.repository.findError = errors.New("database unavailable")
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
	handler := fixture.authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called")
	}))
	request := httptest.NewRequest(http.MethodGet, "/widget/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer "+rawToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func TestPrincipalContextHelpers(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := PrincipalFromContext(request.Context()); ok {
		t.Fatal("PrincipalFromContext() unexpectedly found a principal")
	}
	want := Principal{AccountID: 42, UserID: 99, TokenID: "token"}
	got, ok := PrincipalFromContext(ContextWithPrincipal(request.Context(), want))
	if !ok || got != want {
		t.Fatalf("PrincipalFromContext() = %+v/%v, want %+v/true", got, ok, want)
	}
}
