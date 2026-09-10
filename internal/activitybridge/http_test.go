package activitybridge

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestPublicInputCannotSupplyTenantOrAdminIdentity(t *testing.T) {
	for _, body := range []string{`{"kind":"sync","installation_id":"other"}`, `{"kind":"sync","user_id":1}`, `{"kind":"sync","is_admin":true}`, `{"kind":"sync","retention_days":30}`, `{} {}`, `null`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var input SyncInput
		if err := decode(httptest.NewRecorder(), r, &input); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("accepted identity/config injection %s", body)
		}
	}
	for _, query := range []string{"from=1&to=2&installation_id=other", "from=1&to=2&user_ids=1,1", "from=1&to=99999999", "from=1&to=2&limit=101", "from=1&to=2&from=2"} {
		if _, err := decodeQuery(httptest.NewRequest("GET", "/?"+query, nil)); err == nil {
			t.Fatalf("accepted query %s", query)
		}
	}
}

func TestStableErrorMapping(t *testing.T) {
	for code, status := range map[serviceapi.Code]int{
		serviceapi.InvalidArgument: 400, serviceapi.Unauthenticated: 401, serviceapi.PermissionDenied: 403,
		serviceapi.ReauthRequired: 403, serviceapi.NotFound: 404, serviceapi.Conflict: 409,
		serviceapi.ResourceExhausted: 429, serviceapi.Unavailable: 503, serviceapi.DeadlineExceeded: 504,
	} {
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-ID", "11111111-1111-1111-1111-111111111111")
		writeError(w, serviceapi.Fail(code, "sensitive upstream detail"))
		if w.Code != status || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatalf("%s -> %d %s", code, w.Code, w.Body.String())
		}
		var payload struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s decode %v body=%s", code, err, w.Body.String())
		}
		if payload.Error.Code != string(code) || payload.Error.RequestID != "11111111-1111-1111-1111-111111111111" || payload.Error.Message == "" {
			t.Fatalf("%s envelope=%+v", code, payload.Error)
		}
		wantRetryable := code == serviceapi.Unavailable || code == serviceapi.ResourceExhausted || code == serviceapi.DeadlineExceeded
		if payload.Error.Retryable != wantRetryable {
			t.Fatalf("%s retryable=%t", code, payload.Error.Retryable)
		}
		if code == serviceapi.ReauthRequired && w.Code == 503 {
			t.Fatal("reauth_required mapped to unavailable")
		}
		if code == serviceapi.Unauthenticated && w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("unauthenticated missing challenge")
		}
		if code == serviceapi.ReauthRequired && w.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("reauth_required must not use the widget JWT challenge")
		}
	}
}
