package admincrm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

const testToken = "admin-dev-credential"

func serve(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Admin-Actor", "employee:11111111-1111-4111-8111-111111111111")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body
}

func TestUsersRouteReturnsProjection(t *testing.T) {
	fetched := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{users: []amocrm.CRMUser{crmUser(10, "Первый", true, true), crmUser(11, "Второй", false, false)}}
	service := &Service{Resolver: &fakeResolver{connection: testConnection()}, CRM: reader, Now: func() time.Time { return fetched }}
	router := Router(nil, testToken, service)

	rec := serve(t, router, "/admin/v1/accounts/31415926/crm-users", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("headers=%v", rec.Header())
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{"email", "rights", "group", "role", "leads", "token", "secret", "phone"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("response mentions %q: %s", forbidden, raw)
		}
	}
	body := decode(t, rec)
	if body["state"] != StateOK || body["source"] != "core" || body["account_id"] != float64(31415926) || body["total"] != float64(2) {
		t.Fatalf("body=%s", raw)
	}
	if body["observed_at"] != "2026-11-06T10:00:00Z" || body["fresh_until"] != "2026-11-06T10:05:00Z" || body["retry_after_seconds"] != nil {
		t.Fatalf("times=%s", raw)
	}
	connection, _ := body["connection"].(map[string]any)
	if connection["installation_id"] != testConnection().InstallationID.String() || connection["integration_code"] != "widget-fixture-a" {
		t.Fatalf("connection=%v", connection)
	}
	items, _ := body["items"].([]any)
	first, _ := items[0].(map[string]any)
	if len(items) != 2 || first["id"] != float64(10) || first["name"] != "Первый" || first["is_active"] != true || first["is_admin"] != true || len(first) != 4 {
		t.Fatalf("items=%v", items)
	}
}

func TestUsersRouteStates(t *testing.T) {
	now := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		resolveErr error
		readErr    error
		state      string
		connection bool
		retry      any
	}{
		{"no connection", ErrNoAuthorizedConnection, nil, StateNoAuthorizedConnection, false, nil},
		{"auth", nil, &amocrm.APIError{Kind: amocrm.ErrorUnauthorized, StatusCode: 401}, StateAuthError, true, nil},
		{"rate", nil, &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429, RetryAfter: 1500 * time.Millisecond}, StateRateLimited, true, float64(2)},
		{"incomplete", nil, amocrm.ErrIncompleteResponse, StateIncomplete, true, nil},
		{"network", nil, amocrm.ErrTransport, StateNetworkError, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{Resolver: &fakeResolver{connection: testConnection(), err: tc.resolveErr}, CRM: &fakeReader{err: tc.readErr}, Now: func() time.Time { return now }}
			rec := serve(t, Router(nil, testToken, service), "/admin/v1/accounts/7/crm-users", testToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
			}
			body := decode(t, rec)
			if body["state"] != tc.state || body["items"] != nil || body["total"] != nil || body["fresh_until"] != nil || body["observed_at"] != "2026-11-06T11:00:00Z" {
				t.Fatalf("body=%s", rec.Body.String())
			}
			if (body["connection"] != nil) != tc.connection || body["retry_after_seconds"] != tc.retry {
				t.Fatalf("body=%s", rec.Body.String())
			}
		})
	}
}

func TestUsersRouteErrors(t *testing.T) {
	service := &Service{Resolver: &fakeResolver{err: ErrAccountNotFound}, CRM: &fakeReader{}}
	router := Router(nil, testToken, service)
	for _, tc := range []struct {
		path, token, code string
		status            int
	}{
		{"/admin/v1/accounts/7/crm-users", "", "unauthenticated", http.StatusUnauthorized},
		{"/admin/v1/accounts/7/crm-users", "wrong", "unauthenticated", http.StatusUnauthorized},
		{"/admin/v1/accounts/7/crm-users", testToken, "not_found", http.StatusNotFound},
		{"/admin/v1/accounts/0/crm-users", testToken, "invalid_argument", http.StatusBadRequest},
		{"/admin/v1/accounts/007/crm-users", testToken, "invalid_argument", http.StatusBadRequest},
		{"/admin/v1/accounts/7/crm-users?refresh=true", testToken, "invalid_argument", http.StatusBadRequest},
		{"/admin/v1/accounts", testToken, "not_found", http.StatusNotFound},
	} {
		rec := serve(t, router, tc.path, tc.token)
		body := decode(t, rec)
		envelope, _ := body["error"].(map[string]any)
		if rec.Code != tc.status || envelope["code"] != tc.code || envelope["request_id"] == "" {
			t.Fatalf("%s token=%q status=%d %s", tc.path, tc.token, rec.Code, rec.Body.String())
		}
	}

	failing := Router(nil, testToken, &Service{Resolver: &fakeResolver{err: errors.New("pool closed")}, CRM: &fakeReader{}})
	rec := serve(t, failing, "/admin/v1/accounts/7/crm-users", testToken)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pool closed") {
		t.Fatalf("internal=%d %s", rec.Code, rec.Body.String())
	}
}
