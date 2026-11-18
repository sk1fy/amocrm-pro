package companycrm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func request(identity, path, method string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	if identity != "" {
		u, _ := url.Parse(identity)
		cert := &x509.Certificate{URIs: []*url.URL{u}}
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	return r
}
func TestExactMethodACLRejectsHeadersAndOldActor(t *testing.T) {
	b := binding()
	f := &fakeCRM{}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f}
	for _, tc := range []struct {
		id, path, method string
		want             int
	}{
		{"spiffe://amocrm-pro/admin-company-directory", "/v1/company-crm/staff", "GET", 200},
		{"spiffe://amocrm-pro/admin-company-directory", "/v1/company-crm/clients", "GET", 403},
		{"spiffe://amocrm-pro/platform-company-crm", "/v1/company-crm/clients", "GET", 200},
		{"spiffe://amocrm-pro/platform-company-crm", "/v1/company-crm/staff", "GET", 403},
		{"spiffe://amocrm-pro/team-os", "/v1/company-crm/staff", "GET", 403},
		{"spiffe://amocrm-pro/admin-company-directory?x=1", "/v1/company-crm/staff", "GET", 403},
		{"", "/v1/company-crm/staff", "GET", 403},
		{"spiffe://amocrm-pro/admin-company-directory", "/v1/company-crm/staff?account=123", "GET", 400},
	} {
		r := request(tc.id, tc.path, tc.method)
		r.Header.Set("X-Actor", "admin-company-directory")
		w := httptest.NewRecorder()
		Router(s).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s got=%d want=%d", tc.id, tc.path, w.Code, tc.want)
		}
	}
}
