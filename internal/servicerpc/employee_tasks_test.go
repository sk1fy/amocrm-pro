package servicerpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type taskGateway struct {
	serviceapi.Gateway
	err error
}

func (g taskGateway) OwnTasks(_ context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	if g.err != nil {
		return serviceapi.TaskQueue{}, g.err
	}
	return serviceapi.TaskQueue{Tasks: []serviceapi.QueueTask{{ID: 1, EntityID: 3, EntityType: "leads", EntityName: "Deal", Text: "Call", CompleteTill: 100, TaskTypeID: 2, TaskTypeName: "Звонок", Related: "Anna", UpdatedAt: 50}}, FetchedAt: 10, FreshUntil: 40, Complete: true, Stale: true, ErrorCode: "resource_exhausted", RetryAfterSeconds: r.LeadID}, nil
}
func (g taskGateway) OwnTask(_ context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	return serviceapi.TaskDetail{ID: r.TaskID, ResponsibleUserID: 7, IsCompleted: r.WithContext, ResultText: "ok", Entity: serviceapi.EntityContext{ID: 3, EntityType: "leads", Name: "Deal", PipelineID: 11, PipelineName: "Sales", StatusID: 12, StatusName: "New", StatusColor: "#fff"}}, nil
}
func (g taskGateway) CompleteOwnTask(_ context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	return serviceapi.TaskDetail{ID: r.TaskID, IsCompleted: true, ResultText: r.Result}, nil
}
func (g taskGateway) OwnLead(_ context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	return serviceapi.EntityContext{ID: r.LeadID, EntityType: "leads", Restricted: true}, nil
}

func TestEmployeeTaskRPCRoundTripOverMTLS(t *testing.T) {
	ca := newCA(t)
	address := start(t, ca, &Endpoints{Gateway: taskGateway{}})
	tasks, ok := dialTest(t, ca, address, serviceapi.ActivityService).Gateway.(serviceapi.EmployeeTasks)
	if !ok {
		t.Fatal("Gateway client lacks employee tasks")
	}
	ctx := context.Background()
	queue, err := tasks.OwnTasks(ctx, serviceapi.OwnTasksRequest{LeadID: 9})
	want := serviceapi.QueueTask{ID: 1, EntityID: 3, EntityType: "leads", EntityName: "Deal", Text: "Call", CompleteTill: 100, TaskTypeID: 2, TaskTypeName: "Звонок", Related: "Anna", UpdatedAt: 50}
	if err != nil || len(queue.Tasks) != 1 || queue.Tasks[0] != want || queue.FetchedAt != 10 || queue.FreshUntil != 40 || !queue.Complete || !queue.Stale || queue.ErrorCode != "resource_exhausted" || queue.RetryAfterSeconds != 9 {
		t.Fatalf("queue %+v %v", queue, err)
	}
	detail, err := tasks.OwnTask(ctx, serviceapi.OwnTaskRequest{TaskID: 4, WithContext: true})
	if err != nil || detail.ID != 4 || !detail.IsCompleted || detail.Entity.StatusColor != "#fff" || detail.Entity.PipelineName != "Sales" {
		t.Fatalf("detail %+v %v", detail, err)
	}
	done, err := tasks.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{TaskID: 4, Result: "Готово"})
	if err != nil || done.ResultText != "Готово" {
		t.Fatalf("complete %+v %v", done, err)
	}
	lead, err := tasks.OwnLead(ctx, serviceapi.OwnLeadRequest{LeadID: 5})
	if err != nil || !lead.Restricted || lead.ID != 5 {
		t.Fatalf("lead %+v %v", lead, err)
	}
	events := dialTest(t, ca, address, serviceapi.EventsService).Gateway.(serviceapi.EmployeeTasks)
	if _, err := events.OwnTasks(ctx, serviceapi.OwnTasksRequest{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("CRM Events reached employee tasks: %v", err)
	}
}

func TestEmployeeTaskRPCKeepsRetryAfterAndEmptyQueue(t *testing.T) {
	ca := newCA(t)
	limited := start(t, ca, &Endpoints{Gateway: taskGateway{err: &serviceapi.Error{Code: serviceapi.ResourceExhausted, Message: "budget", RetryAfter: 7 * time.Second}}})
	tasks := dialTest(t, ca, limited, serviceapi.ActivityService).Gateway.(serviceapi.EmployeeTasks)
	_, err := tasks.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{})
	var domain *serviceapi.Error
	if !errors.As(err, &domain) || domain.Code != serviceapi.ResourceExhausted || domain.RetryAfter != 7*time.Second {
		t.Fatalf("retry info lost: %v", err)
	}
	if got := taskQueueFromPB(nil); got.Tasks == nil {
		t.Fatal("empty queue must decode to an empty list")
	}
}
