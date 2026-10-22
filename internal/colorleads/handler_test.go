package colorleads

import (
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingStore struct {
	called bool
	p      widgetauth.Principal
	f      Fence
}

func (s *recordingStore) ReadSettings(_ context.Context, p widgetauth.Principal, f Fence) (SettingsDocument, error) {
	s.called = true
	s.p = p
	s.f = f
	return SettingsDocument{Settings: json.RawMessage(`{}`), Revision: 3}, nil
}
func (s *recordingStore) WriteSettings(_ context.Context, p widgetauth.Principal, f Fence, raw json.RawMessage, n int64) (SettingsDocument, error) {
	s.called = true
	s.p = p
	s.f = f
	if ValidateSettings(raw) != nil {
		return SettingsDocument{}, ErrInvalid
	}
	return SettingsDocument{raw, n + 1}, nil
}
func (s *recordingStore) ReadColors(context.Context, widgetauth.Principal, Fence, []int64) (ColorsDocument, error) {
	s.called = true
	return ColorsDocument{}, nil
}
func (s *recordingStore) WriteColors(context.Context, widgetauth.Principal, Fence, []Color, int64) (ColorsDocument, error) {
	s.called = true
	return ColorsDocument{}, nil
}
func TestHTTPBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		auth       bool
		epoch      string
		want       int
	}{
		{"missing auth", `{"settings":{},"expected_revision":0}`, false, "1", 401},
		{"missing fence", `{"settings":{},"expected_revision":0}`, true, "", 409},
		{"spoof account", `{"settings":{},"expected_revision":0,"amo_account_id":999}`, true, "1", 400},
		{"missing revision", `{"settings":{}}`, true, "1", 400},
		{"trailing JSON", `{"settings":{},"expected_revision":0}{}`, true, "1", 400},
		{"valid", `{"settings":{"main_settings":[]},"expected_revision":0}`, true, "1", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &recordingStore{}
			r := chi.NewRouter()
			NewHandler(s).Register(r)
			req := httptest.NewRequest("PUT", "/v1/widgets/color-leads/settings", strings.NewReader(tc.body))
			req.Header.Set("X-Routing-Epoch", tc.epoch)
			req.Header.Set("X-Installation-Generation", "2")
			p := widgetauth.Principal{AccountID: 123, InstallationID: uuid.New()}
			if tc.auth {
				req = req.WithContext(widgetauth.ContextWithPrincipal(req.Context(), p))
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if tc.want != 200 && s.called {
				t.Fatal("invalid request reached store")
			}
			if tc.want == 200 && (s.p.AccountID != 123 || s.f.Generation != 2) {
				t.Fatal("principal/fence lost")
			}
		})
	}
}
func TestValidatorsAndFailClosed(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"main_settings":null}`, `{"main_settings":[1]}`, `{"x":1,"x":2}`, `{"main_settings":[{"id":1,"id":2}]}`} {
		if ValidateSettings(json.RawMessage(raw)) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if ValidateSettings(json.RawMessage(`{"main_settings":[{"conditions":[{"fieldId":1,"operator":"eq","value":"x"}]}],"legacy_key":true}`)) != nil {
		t.Fatal("lossless legacy settings rejected")
	}
	rows := []Color{{LeadID: 1, StageID: 2, PipelineID: 3}, {LeadID: 1, StageID: 2, PipelineID: 3}}
	if ValidateColors(rows) == nil {
		t.Fatal("duplicate composite key")
	}
	rows[1].StageID = 4
	if ValidateColors(rows) != nil {
		t.Fatal("distinct stage snapshots rejected")
	}
	s := NewStore(nil, nil)
	if _, e := s.WriteSettings(context.Background(), widgetauth.Principal{}, Fence{}, json.RawMessage(`{}`), 0); e != ErrNotConfigured {
		t.Fatal(e)
	}
	if _, e := s.ImportSnapshot(context.Background(), nil, ImportCommand{}); e != ErrNotConfigured {
		t.Fatal(e)
	}
}
