package activity

import (
	"context"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type panelStore struct {
	*memorySettings
	settings serviceapi.TaskPanelSettings
}

func (s panelStore) CurrentSession(context.Context, serviceapi.Scope, int64) (serviceapi.WorkSession, error) {
	return serviceapi.WorkSession{}, nil
}
func (s panelStore) ApplySession(context.Context, serviceapi.Principal, serviceapi.SessionCommand) (serviceapi.WorkSession, error) {
	return serviceapi.WorkSession{}, nil
}
func (s panelStore) ReadSessions(context.Context, serviceapi.Scope, int64, int64, int64, string) (serviceapi.SessionPage, error) {
	return serviceapi.SessionPage{}, nil
}
func (s panelStore) TaskPanel(context.Context, serviceapi.Scope) (serviceapi.TaskPanelSettings, error) {
	return s.settings, nil
}
func (s panelStore) SaveTaskPanel(context.Context, serviceapi.Scope, serviceapi.TaskPanelSettings) (serviceapi.TaskPanelSettings, error) {
	return s.settings, nil
}

type taskGatewayFake struct {
	fakeGateway
	queues    []serviceapi.OwnTasksRequest
	completes int
	reads     int
}

func (g *taskGatewayFake) OwnTasks(_ context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	g.queues = append(g.queues, r)
	return serviceapi.TaskQueue{Tasks: []serviceapi.QueueTask{}}, nil
}
func (g *taskGatewayFake) OwnTask(context.Context, serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	g.reads++
	return serviceapi.TaskDetail{}, nil
}
func (g *taskGatewayFake) CompleteOwnTask(context.Context, serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	g.completes++
	return serviceapi.TaskDetail{}, nil
}
func (g *taskGatewayFake) OwnLead(context.Context, serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	return serviceapi.EntityContext{}, nil
}

func TestEmployeeTasksFollowTaskPanelEnrolment(t *testing.T) {
	gateway := &taskGatewayFake{}
	auth := serviceapi.Auth{Token: "verified"}
	disabled := New(panelStore{memorySettings: &memorySettings{}, settings: serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{8}}}, &fakePolicy{kind: serviceapi.PrincipalKindEmployee}, &fakeEvents{}, gateway)
	if _, err := disabled.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("unenrolled queue: %v", err)
	}
	if _, err := disabled.CompleteOwnTask(context.Background(), serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "x"}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || gateway.completes != 0 {
		t.Fatalf("unenrolled completion: %v", err)
	}
	// An interval left from an enabled period must still reconcile its task.
	if _, err := disabled.OwnTask(context.Background(), serviceapi.OwnTaskRequest{Auth: auth, TaskID: 1}); err != nil || gateway.reads != 1 {
		t.Fatalf("disabled task read: %v", err)
	}

	enabled := New(panelStore{memorySettings: &memorySettings{}, settings: serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7}}}, &fakePolicy{kind: serviceapi.PrincipalKindEmployee}, &fakeEvents{}, gateway)
	if _, err := enabled.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth, LeadID: 3}); err != nil || gateway.queues[0].LeadID != 0 {
		t.Fatalf("related deals off must not read the open lead: %+v %v", gateway.queues, err)
	}
	related := New(panelStore{memorySettings: &memorySettings{}, settings: serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7}, IncludeRelatedDeals: true}}, &fakePolicy{kind: serviceapi.PrincipalKindEmployee}, &fakeEvents{}, gateway)
	if _, err := related.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth, LeadID: 3}); err != nil || gateway.queues[1].LeadID != 3 {
		t.Fatalf("related deals on: %+v %v", gateway.queues, err)
	}
	admin := New(panelStore{memorySettings: &memorySettings{}, settings: serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7}}}, &fakePolicy{}, &fakeEvents{}, gateway)
	if _, err := admin.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("non-employee principal: %v", err)
	}
}
