package widgetcors

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const activeOrigin = "https://tenant.amocrm.ru"

type stubAuthorizer struct {
	active bool
	err    error
	calls  int
}

func (authorizer *stubAuthorizer) IsActiveOrigin(_ context.Context, origin string) (bool, error) {
	authorizer.calls++
	if origin != activeOrigin {
		return false, nil
	}
	return authorizer.active, authorizer.err
}

func TestPreflightReflectsOnlyStrictActiveOriginContract(t *testing.T) {
	authorizer := &stubAuthorizer{active: true}
	handler := Middleware(authorizer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight reached application handler")
	}))
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/widget/actions/leads/set-status", nil)
	request.Header.Set("Origin", activeOrigin)
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "idempotency-key, Authorization, content-type, X-Request-ID")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	assertHeader(t, response.Header(), allowOriginHeader, activeOrigin)
	assertHeader(t, response.Header(), allowMethodsHeader, http.MethodPost)
	assertHeader(t, response.Header(), allowHeadersHeader, "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
	assertHeader(t, response.Header(), "Access-Control-Max-Age", "300")
	if response.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("credentials must not be enabled")
	}
	assertVary(t, response.Header(), "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
}

func TestPreflightRejectsUntrustedMethodHeaderAndOrigin(t *testing.T) {
	tests := map[string]struct {
		authorizer OriginAuthorizer
		origin     string
		method     string
		headers    string
		wantStatus int
	}{
		"inactive origin": {
			authorizer: &stubAuthorizer{}, origin: activeOrigin,
			method: http.MethodPost, wantStatus: http.StatusForbidden,
		},
		"non HTTPS origin": {
			authorizer: &stubAuthorizer{active: true}, origin: "http://tenant.amocrm.ru",
			method: http.MethodPost, wantStatus: http.StatusForbidden,
		},
		"non canonical origin": {
			authorizer: &stubAuthorizer{active: true}, origin: "https://TENANT.amocrm.ru",
			method: http.MethodPost, wantStatus: http.StatusForbidden,
		},
		"unsupported method": {
			authorizer: &stubAuthorizer{active: true}, origin: activeOrigin,
			method: http.MethodDelete, wantStatus: http.StatusForbidden,
		},
		"unsupported header": {
			authorizer: &stubAuthorizer{active: true}, origin: activeOrigin,
			method: http.MethodGet, headers: "X-Unsafe", wantStatus: http.StatusForbidden,
		},
		"storage failure": {
			authorizer: &stubAuthorizer{active: true, err: errors.New("database unavailable")},
			origin:     activeOrigin, method: http.MethodGet, wantStatus: http.StatusServiceUnavailable,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			handler := Middleware(test.authorizer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("rejected preflight reached application handler")
			}))
			request := httptest.NewRequest(http.MethodOptions, "/api/v1/widget/bootstrap", nil)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Access-Control-Request-Method", test.method)
			if test.headers != "" {
				request.Header.Set("Access-Control-Request-Headers", test.headers)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if response.Header().Get(allowOriginHeader) != "" {
				t.Fatal("rejected response must not grant an origin")
			}
			assertVary(t, response.Header(), "Origin")
		})
	}
}

func TestActualBrowserResponseAndNonBrowserPassThrough(t *testing.T) {
	authorizer := &stubAuthorizer{active: true}
	handler := Middleware(authorizer)(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusAccepted)
	}))

	browserRequest := httptest.NewRequest(http.MethodPost, "/api/v1/widget/actions/ping", nil)
	browserRequest.Header.Set("Origin", activeOrigin)
	browserResponse := httptest.NewRecorder()
	handler.ServeHTTP(browserResponse, browserRequest)
	if browserResponse.Code != http.StatusAccepted {
		t.Fatalf("browser status = %d", browserResponse.Code)
	}
	assertHeader(t, browserResponse.Header(), allowOriginHeader, activeOrigin)
	assertHeader(t, browserResponse.Header(), exposeHeadersHeader, "X-Request-ID, Idempotency-Replayed")

	nonBrowserResponse := httptest.NewRecorder()
	handler.ServeHTTP(nonBrowserResponse, httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil))
	if nonBrowserResponse.Code != http.StatusAccepted {
		t.Fatalf("non-browser status = %d", nonBrowserResponse.Code)
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer calls = %d, want 1", authorizer.calls)
	}
}

func TestBindPrincipalIssuer(t *testing.T) {
	principal := widgetauth.Principal{
		IntegrationID: uuid.New(), InstallationID: uuid.New(), AccountID: 42,
		UserID: 7, Issuer: activeOrigin,
	}
	handler := BindPrincipalIssuer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))

	tests := map[string]struct {
		origin        string
		withPrincipal bool
		wantStatus    int
	}{
		"matching":      {origin: activeOrigin, withPrincipal: true, wantStatus: http.StatusNoContent},
		"different":     {origin: "https://other.amocrm.ru", withPrincipal: true, wantStatus: http.StatusForbidden},
		"missing actor": {origin: activeOrigin, wantStatus: http.StatusForbidden},
		"non browser":   {withPrincipal: true, wantStatus: http.StatusNoContent},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil)
			request.Header.Set("Origin", test.origin)
			if test.withPrincipal {
				request = request.WithContext(widgetauth.ContextWithPrincipal(request.Context(), principal))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func assertHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	if got := header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func assertVary(t *testing.T, header http.Header, wants ...string) {
	t.Helper()
	joined := strings.Join(header.Values("Vary"), ",")
	for _, want := range wants {
		if !strings.Contains(strings.ToLower(joined), strings.ToLower(want)) {
			t.Fatalf("Vary = %q, missing %q", joined, want)
		}
	}
}
