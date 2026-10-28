package activitybridge

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// taskAuth delegates the verified widget employee. The browser never names
// the employee, task owner or installation; Gateway binds tasks to ActorID.
func (b *Bridge) taskAuth(ctx context.Context, p widgetauth.Principal, complete bool) (serviceapi.Auth, error) {
	grants := []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionOwnTasks}}
	if complete {
		grants = append(grants, serviceapi.Grant{Audience: serviceapi.GatewayService, Action: serviceapi.ActionCompleteTask})
	}
	return b.policy.Issue(ctx, serviceapi.IssueRequest{Scope: serviceapi.Scope{InstallationID: p.InstallationID, IntegrationID: p.IntegrationID}, ActorID: p.UserID, Kind: serviceapi.PrincipalKindEmployee, Consumer: serviceapi.ActivityService, RequestID: uuid.NewString(), Grants: grants})
}

func (b *Bridge) employeeTasks() (serviceapi.EmployeeTasks, error) {
	tasks, ok := b.activity.(serviceapi.EmployeeTasks)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "employee tasks unavailable")
	}
	return tasks, nil
}

// taskQuery accepts only the named optional parameters, each at most once.
func taskQuery(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, serviceapi.Fail(serviceapi.InvalidArgument, "malformed query parameters")
	}
	for key, value := range values {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known || len(value) != 1 {
			return nil, serviceapi.Fail(serviceapi.InvalidArgument, "unknown or repeated query parameter")
		}
	}
	return values, nil
}

func (b *Bridge) OwnTasksHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	values, err := taskQuery(r, "lead_id")
	if err != nil {
		writeError(w, err)
		return
	}
	var request serviceapi.OwnTasksRequest
	if raw := values.Get("lead_id"); raw != "" {
		if request.LeadID, err = strconv.ParseInt(raw, 10, 64); err != nil || request.LeadID <= 0 {
			writeError(w, serviceapi.Fail(serviceapi.InvalidArgument, "lead_id must be a positive ID"))
			return
		}
	}
	tasks, err := b.employeeTasks()
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if request.Auth, err = b.taskAuth(ctx, p, false); err != nil {
		writeError(w, err)
		return
	}
	result, err := tasks.OwnTasks(ctx, request)
	if result.Tasks == nil {
		result.Tasks = []serviceapi.QueueTask{}
	}
	respond(w, http.StatusOK, result, err)
}

func (b *Bridge) OwnTaskHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, err := pathInt64(r, "taskID")
	if err != nil {
		writeError(w, err)
		return
	}
	values, err := taskQuery(r, "context")
	if err != nil {
		writeError(w, err)
		return
	}
	request := serviceapi.OwnTaskRequest{TaskID: id}
	if raw, present := values["context"]; present {
		if raw[0] != "true" && raw[0] != "false" {
			writeError(w, serviceapi.Fail(serviceapi.InvalidArgument, "context must be true or false"))
			return
		}
		request.WithContext = raw[0] == "true"
	}
	tasks, err := b.employeeTasks()
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if request.Auth, err = b.taskAuth(ctx, p, false); err != nil {
		writeError(w, err)
		return
	}
	result, err := tasks.OwnTask(ctx, request)
	respond(w, http.StatusOK, result, err)
}

// CompleteTaskHTTP needs no Idempotency-Key: Gateway rereads the task and a
// repeat after a lost response returns the completed task without a write.
func (b *Bridge) CompleteTaskHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, err := pathInt64(r, "taskID")
	if err != nil {
		writeError(w, err)
		return
	}
	var input struct {
		Result string `json:"result"`
	}
	if err := decode(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	request := serviceapi.CompleteTaskRequest{TaskID: id, Result: input.Result}
	if err := serviceapi.ValidateTaskResult(request.Result); err != nil {
		writeError(w, err)
		return
	}
	tasks, err := b.employeeTasks()
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if request.Auth, err = b.taskAuth(ctx, p, true); err != nil {
		writeError(w, err)
		return
	}
	result, err := tasks.CompleteOwnTask(ctx, request)
	respond(w, http.StatusOK, result, err)
}

func (b *Bridge) OwnLeadHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, err := pathInt64(r, "leadID")
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, serviceapi.Fail(serviceapi.InvalidArgument, "lead context accepts no query parameters"))
		return
	}
	tasks, err := b.employeeTasks()
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	request := serviceapi.OwnLeadRequest{LeadID: id}
	if request.Auth, err = b.taskAuth(ctx, p, false); err != nil {
		writeError(w, err)
		return
	}
	result, err := tasks.OwnLead(ctx, request)
	respond(w, http.StatusOK, result, err)
}
