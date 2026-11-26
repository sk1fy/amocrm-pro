package productplatform

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func TestPrivateAccountLicenseRequiresCoreIdentity(t *testing.T) {
	handler := PrivateHandler(LifecycleHandler(nil), nil)
	for _, tc := range []struct {
		name     string
		identity string
		status   int
	}{
		{"anonymous", "", http.StatusForbidden},
		{"manager", managerBFFIdentity, http.StatusForbidden},
		{"core", coreIdentity, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, wire.AccountLicensePath, strings.NewReader(`{"hints":{"tariff_id":22000000}}`))
			request.Header.Set("Content-Type", "application/json")
			if tc.identity != "" {
				identity, err := url.Parse(tc.identity)
				if err != nil {
					t.Fatal(err)
				}
				leaf := &x509.Certificate{URIs: []*url.URL{identity}}
				request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body)
			}
		})
	}
}
