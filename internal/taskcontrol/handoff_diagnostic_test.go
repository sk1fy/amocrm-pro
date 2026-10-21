package taskcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type unavailableIdentity struct{ err error }

func (c unavailableIdentity) DistributionUser(context.Context, uuid.UUID, int64) (amocrm.DistributionUser, error) {
	return amocrm.DistributionUser{}, c.err
}

func TestHandoffUnavailableDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		err          error
		status       int
	}{
		{"deadline", "deadline_exceeded", context.DeadlineExceeded, 0},
		{"cancel", "request_canceled", context.Canceled, 0},
		{"budget", "crm_budget_overloaded", &amocrm.APIError{Kind: amocrm.ErrorOverloaded}, 0},
		{"rate limit", "crm_http_error", &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429}, 429},
		{"CRM outage", "crm_http_error", &amocrm.APIError{Kind: amocrm.ErrorTemporary, StatusCode: 503}, 503},
		{"transport", "crm_transport_error", amocrm.ErrTransport, 0},
		{"incomplete", "crm_response_incomplete", amocrm.ErrIncompleteResponse, 0},
		{"unknown", "internal_error", errors.New("sensitive upstream detail"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := &HandoffHandler{CRM: unavailableIdentity{fmt.Errorf("secret-token: %w", tc.err)}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/widget/task-control/auth", strings.NewReader(`{"purpose":"runtime","requestId":"`+uuid.NewString()+`"}`))
			r.Header.Set("X-Request-ID", uuid.NewString())
			r.Header.Set("X-Auth-Token", "secret-token")
			r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), widgetauth.Principal{UserID: 7, InstallationID: uuid.New()}))
			w := httptest.NewRecorder()
			httpmiddleware.RequestID(h).ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry["request_id"] != r.Header.Get("X-Request-ID") || entry["purpose"] != "runtime" || entry["stage"] != "crm_identity" || entry["reason_code"] != tc.reason || entry["upstream_status"] != float64(tc.status) {
				t.Fatalf("unexpected diagnostic: %v", entry)
			}
			for _, secret := range []string{"secret-token", "sensitive upstream detail"} {
				if strings.Contains(logs.String()+w.Body.String(), secret) {
					t.Fatal("diagnostic exposed sensitive error details")
				}
			}
		})
	}
}
