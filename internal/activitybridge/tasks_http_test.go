package activitybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type issuedPolicy struct {
	serviceapi.Policy
	requests []serviceapi.IssueRequest
}

func (p *issuedPolicy) Issue(_ context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	p.requests = append(p.requests, r)
	return serviceapi.Auth{Token: "delegated"}, nil
}

type taskActivity struct {
	serviceapi.Activity
	queue    serviceapi.OwnTasksRequest
	complete serviceapi.CompleteTaskRequest
	task     serviceapi.OwnTaskRequest
	err      error
}

func (a *taskActivity) OwnTasks(_ context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	a.queue = r
	return serviceapi.TaskQueue{}, a.err
}
func (a *taskActivity) OwnTask(_ context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	a.task = r
	return serviceapi.TaskDetail{ID: r.TaskID}, a.err
}
func (a *taskActivity) CompleteOwnTask(_ context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	a.complete = r
	return serviceapi.TaskDetail{ID: r.TaskID, IsCompleted: true}, a.err
}
func (a *taskActivity) OwnLead(_ context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	return serviceapi.EntityContext{ID: r.LeadID}, a.err
}

func serveTask(t *testing.T, b *Bridge, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	pass := func(next http.Handler) http.Handler { return next }
	b.RegisterHTTP(router, pass, pass)
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	p := widgetauth.Principal{IntegrationID: uuid.New(), InstallationID: uuid.New(), UserID: 7}
	r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestTaskRoutesDelegateVerifiedEmployeeOnly(t *testing.T) {
	policy := &issuedPolicy{}
	activity := &taskActivity{}
	b := New(nil, policy, activity, nil)
	w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks?lead_id=3", "")
	if w.Code != 200 || activity.queue.LeadID != 3 || activity.queue.Auth.Token != "delegated" {
		t.Fatalf("queue %d %s %+v", w.Code, w.Body, activity.queue)
	}
	var queue struct {
		Tasks []any `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &queue); err != nil || queue.Tasks == nil {
		t.Fatalf("empty queue must be an array: %s", w.Body)
	}
	issued := policy.requests[0]
	if issued.Kind != serviceapi.PrincipalKindEmployee || issued.ActorID != 7 || len(issued.Grants) != 2 {
		t.Fatalf("queue delegation %+v", issued)
	}
	if w := serveTask(t, b, "POST", "/api/v1/widget/activity/tasks/9/complete", `{"result":"Done"}`); w.Code != 200 || activity.complete.TaskID != 9 || activity.complete.Result != "Done" {
		t.Fatalf("complete %d %s", w.Code, w.Body)
	}
	last := policy.requests[len(policy.requests)-1]
	if len(last.Grants) != 3 || last.Grants[2].Action != serviceapi.ActionCompleteTask {
		t.Fatalf("complete delegation %+v", last.Grants)
	}
	if w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks/9?context=true", ""); w.Code != 200 || !activity.task.WithContext {
		t.Fatalf("detail %d %s", w.Code, w.Body)
	}
	before := len(policy.requests)
	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/v1/widget/activity/tasks?responsible_user_id=8", ""},
		{"GET", "/api/v1/widget/activity/tasks?lead_id=-1", ""},
		{"GET", "/api/v1/widget/activity/tasks?lead_id=1&lead_id=2", ""},
		{"GET", "/api/v1/widget/activity/tasks/9?context=yes", ""},
		{"GET", "/api/v1/widget/activity/leads/3?user_id=8", ""},
		{"POST", "/api/v1/widget/activity/tasks/9/complete", `{"result":"Done","user_id":8}`},
		{"POST", "/api/v1/widget/activity/tasks/9/complete", `{"result":"  "}`},
		{"POST", "/api/v1/widget/activity/tasks/9/complete", `{"result":"` + strings.Repeat("я", 1001) + `"}`},
	} {
		if w := serveTask(t, b, tc.method, tc.target, tc.body); w.Code != 400 {
			t.Fatalf("%s %s accepted: %d", tc.method, tc.target, w.Code)
		}
	}
	if w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks/abc", ""); w.Code != 404 {
		t.Fatalf("non-numeric task %d", w.Code)
	}
	if len(policy.requests) != before {
		t.Fatal("rejected input reached delegation")
	}
}

func TestTaskRoutesForwardUpstreamRetryAfter(t *testing.T) {
	activity := &taskActivity{err: &serviceapi.Error{Code: serviceapi.ResourceExhausted, Message: "amoCRM", RetryAfter: 12500 * time.Millisecond}}
	b := New(nil, &issuedPolicy{}, activity, nil)
	w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks", "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "13" {
		t.Fatalf("429 %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	activity.err = &serviceapi.Error{Code: serviceapi.Unavailable, Message: "refreshing", RetryAfter: time.Second}
	if w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks", ""); w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("503 %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	activity.err = serviceapi.Fail(serviceapi.ResourceExhausted, "local")
	if w := serveTask(t, b, "GET", "/api/v1/widget/activity/tasks", ""); w.Header().Get("Retry-After") != "1" {
		t.Fatalf("default Retry-After=%q", w.Header().Get("Retry-After"))
	}
}
