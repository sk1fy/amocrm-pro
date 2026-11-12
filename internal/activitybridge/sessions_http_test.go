package activitybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type sessionActivity struct {
	serviceapi.Activity
	commands []serviceapi.SessionCommand
	receipt  serviceapi.WorkSession
}

func (a *sessionActivity) ApplySession(_ context.Context, c serviceapi.SessionCommand) (serviceapi.WorkSession, error) {
	a.commands = append(a.commands, c)
	return a.receipt, nil
}

func TestSessionWriteDelegatesTaskStartWithOneGrantRequest(t *testing.T) {
	policy := &issuedPolicy{}
	activity := &sessionActivity{receipt: serviceapi.WorkSession{ID: uuid.NewString(), Kind: "work", Context: serviceapi.SessionContext{EntityType: "contacts", EntityID: 26}}}
	b := New(nil, policy, activity, nil)
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	pass := func(next http.Handler) http.Handler { return next }
	b.RegisterHTTP(router, pass, pass)
	p := widgetauth.Principal{InstallationID: uuid.New(), IntegrationID: uuid.New(), UserID: 7}
	commandID, clientID := uuid.NewString(), uuid.NewString()
	write := func(body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, apicontract.ActivitySessionWrite.Path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	inputContext := `"context":{"task_id":9,"client_id":"` + clientID + `"}`
	valid := `{"action":"start_task","expected_session_id":"",` + inputContext + `}`
	w := write(valid, commandID)
	var receipt serviceapi.WorkSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt != activity.receipt {
		t.Fatalf("task start response: %d %s", w.Code, w.Body)
	}
	if len(policy.requests) != 1 || len(activity.commands) != 1 {
		t.Fatalf("start issued %d delegations and %d commands", len(policy.requests), len(activity.commands))
	}
	issued := policy.requests[0]
	if issued.Scope != (serviceapi.Scope{InstallationID: p.InstallationID, IntegrationID: p.IntegrationID}) || issued.ActorID != p.UserID ||
		issued.Kind != serviceapi.PrincipalKindEmployee || issued.Consumer != serviceapi.ActivityService ||
		issued.RequestID != w.Header().Get("X-Request-ID") ||
		!slices.Equal(issued.Grants, []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionOwnTasks}}) {
		t.Fatalf("unexpected task delegation: %+v", issued)
	}
	got := activity.commands[0]
	if got.Action != serviceapi.SessionActionStartTask || !got.HasExpectedSession || got.ExpectedSessionID != "" || got.CommandID != commandID ||
		got.Context != (serviceapi.SessionContext{TaskID: 9, ClientID: clientID}) || got.Auth.Token != "delegated" {
		t.Fatalf("unexpected task command: %+v", got)
	}

	for name, body := range map[string]string{
		"missing expected":   `{"action":"start_task",` + inputContext + `}`,
		"null expected":      strings.Replace(valid, `"expected_session_id":""`, `"expected_session_id":null`, 1),
		"malformed expected": strings.Replace(valid, `"expected_session_id":""`, `"expected_session_id":"invalid"`, 1),
		"missing client":     `{"action":"start_task","expected_session_id":"","context":{"task_id":9}}`,
		"invalid client":     strings.Replace(valid, clientID, uuid.Nil.String(), 1),
		"invalid task":       strings.Replace(valid, `"task_id":9`, `"task_id":0`, 1),
		"supplied entity":    strings.Replace(valid, `"task_id":9`, `"task_id":9,"entity_id":0`, 1),
		"supplied session":   strings.Replace(valid, `"expected_session_id":""`, `"expected_session_id":"","session_id":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if w := write(body, commandID); w.Code != http.StatusBadRequest {
				t.Fatalf("invalid task start accepted: %d %s", w.Code, w.Body)
			}
		})
	}
	if w := write(valid, "bad-key"); w.Code != http.StatusBadRequest || len(policy.requests) != 1 || len(activity.commands) != 1 {
		t.Fatal("invalid start input reached delegation or Activity")
	}
	if w := write(`{"action":"work","context":{"task_id":9,"entity_id":5,"entity_type":"leads"}}`, uuid.NewString()); w.Code != http.StatusOK {
		t.Fatalf("legacy work rejected: %d %s", w.Code, w.Body)
	}
	if len(policy.requests) != 2 || !slices.Equal(policy.requests[1].Grants, []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}}) ||
		len(activity.commands) != 2 || activity.commands[1].HasExpectedSession || activity.commands[1].Context.EntityID != 5 {
		t.Fatal("legacy work changed its delegation or caller context")
	}
}
