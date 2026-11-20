package companycrm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
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

func TestAuthenticatedColdFailureAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry time.Duration
	}{
		{"wrong_account", amocrm.ErrIncompleteResponse, time.Minute},
		{"rate_limited", &amocrm.APIError{Kind: amocrm.ErrorRateLimited, RetryAfter: 5 * time.Second}, 5 * time.Second},
		{"timeout", context.DeadlineExceeded, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := binding()
			now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
			f := &fakeCRM{verifyErr: tc.err, users: []amocrm.CompanyCRMUser{{ID: 7}}}
			s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f, now: func() time.Time { return now }}
			handler := Router(s)
			read := func(want int) Response[[]StaffMember] {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request("spiffe://amocrm-pro/admin-company-directory", "/v1/company-crm/staff", "GET"))
				if w.Code != want {
					t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
				}
				var response Response[[]StaffMember]
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				return response
			}
			first := read(http.StatusServiceUnavailable)
			if first.Data != nil || first.Source.Complete || first.Source.State != "error" || f.calls != 0 || f.verifies != 1 {
				t.Fatalf("cold failure published data or read dataset: response=%+v staff calls=%d verifies=%d", first, f.calls, f.verifies)
			}
			f.verifyErr = nil
			now = now.Add(tc.retry - time.Second)
			read(http.StatusServiceUnavailable)
			if f.verifies != 1 || f.calls != 0 {
				t.Fatal("retried before existing cooldown elapsed")
			}
			now = now.Add(time.Second)
			recovered := read(http.StatusOK)
			if recovered.Source.State != "fresh" || !recovered.Source.Complete || recovered.Data == nil || len(*recovered.Data) != 1 || f.verifies != 2 || f.calls != 1 {
				t.Fatalf("recovery=%+v verifies=%d staff calls=%d", recovered, f.verifies, f.calls)
			}
		})
	}
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

type dealTransport func(*http.Request) (*http.Response, error)

func (f dealTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallationDealWritesClientFieldsDuringCreation(t *testing.T) {
	b := binding()
	const host = "fixture-company.amocrm.ru"
	episode := uuid.New()
	activeUsers := 0
	input := DealRequest{Episode: episode, Account: b.AccountID, Binding: b.Revision, Title: "Fixture",
		Pipeline: 10, Stage: 20, Create: true, ClientAccountID: 9007199254740993, ActiveUsers: &activeUsers}
	token := &fakeTokens{token: amocrm.AccessToken{
		InstallationID: b.InstallationID, IntegrationID: b.IntegrationID, AccountID: b.AccountID, AccountDomain: host,
	}}
	want := map[int64]string{5: DealMarker(episode), 6: "9007199254740993", 7: "0"}
	calls := 0
	client := amocrm.NewClient(&http.Client{Transport: dealTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.Host != host || r.URL.Path != "/api/v4/leads" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		var leads []struct {
			Fields []struct {
				ID     int64 `json:"field_id"`
				Values []struct {
					Value string `json:"value"`
				} `json:"values"`
			} `json:"custom_fields_values"`
		}
		if err := json.NewDecoder(r.Body).Decode(&leads); err != nil || len(leads) != 1 {
			t.Fatalf("unexpected request body: %+v, %v", leads, err)
		}
		fields := make(map[int64]string)
		for _, field := range leads[0].Fields {
			if len(field.Values) != 1 {
				t.Fatalf("expected one value for field %d: %+v", field.ID, field.Values)
			}
			fields[field.ID] = field.Values[0].Value
		}
		if !maps.Equal(fields, want) {
			t.Fatalf("fields = %v, want %v", fields, want)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"_embedded":{"leads":[{"id":101}]}}`))}, nil
	})}, token)
	deals := &DealService{CRM: client, Host: host, MarkerFieldID: 5, AccountIDFieldID: 6, UserCountFieldID: 7,
		Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil })}
	handler := Router(&Service{Deals: deals})
	create := func() {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		r := request("spiffe://amocrm-pro/platform-company-crm", DealPath, http.MethodPost)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
		}
	}
	create()
	input.ClientAccountID, input.ActiveUsers = 0, nil
	want = map[int64]string{5: DealMarker(episode)}
	create()
	if calls != 2 {
		t.Fatalf("sent %d requests, want 2", calls)
	}
}
