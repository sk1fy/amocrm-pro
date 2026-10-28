package servicerpc

import (
	"context"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
)

// Employee task RPCs exist on both Gateway and Activity with one contract;
// owners that predate it answer unavailable instead of an unknown method.
func employeeTasks(impl any) (serviceapi.EmployeeTasks, error) {
	tasks, ok := impl.(serviceapi.EmployeeTasks)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "employee task access unavailable")
	}
	return tasks, nil
}

func taskQueueFromPB(v *pb.TaskQueue) serviceapi.TaskQueue {
	out := fromTaskQueue(v)
	if out.Tasks == nil {
		out.Tasks = []serviceapi.QueueTask{}
	}
	return out
}

func (s *gatewayServer) OwnTasks(ctx context.Context, r *pb.OwnTasksRequest) (*pb.TaskQueue, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnTasks(ctx, fromOwnTasksRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskQueue(v), nil
}
func (s *gatewayServer) OwnTask(ctx context.Context, r *pb.OwnTaskRequest) (*pb.TaskDetail, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnTask(ctx, fromOwnTaskRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskDetail(v), nil
}
func (s *gatewayServer) CompleteOwnTask(ctx context.Context, r *pb.CompleteTaskRequest) (*pb.TaskDetail, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.CompleteOwnTask(ctx, fromCompleteTaskRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskDetail(v), nil
}
func (s *gatewayServer) OwnLead(ctx context.Context, r *pb.OwnLeadRequest) (*pb.EntityContext, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnLead(ctx, fromOwnLeadRequest(r))
	if err != nil {
		return nil, err
	}
	return toEntityContext(v), nil
}

func (c gatewayClient) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	v, err := c.remote.OwnTasks(ctx, toOwnTasksRequest(r))
	if err != nil {
		return serviceapi.TaskQueue{}, err
	}
	return taskQueueFromPB(v), nil
}
func (c gatewayClient) OwnTask(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	v, err := c.remote.OwnTask(ctx, toOwnTaskRequest(r))
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return fromTaskDetail(v), nil
}
func (c gatewayClient) CompleteOwnTask(ctx context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	v, err := c.remote.CompleteOwnTask(ctx, toCompleteTaskRequest(r))
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return fromTaskDetail(v), nil
}
func (c gatewayClient) OwnLead(ctx context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	v, err := c.remote.OwnLead(ctx, toOwnLeadRequest(r))
	if err != nil {
		return serviceapi.EntityContext{}, err
	}
	return fromEntityContext(v), nil
}

func (s *activityServer) OwnTasks(ctx context.Context, r *pb.OwnTasksRequest) (*pb.TaskQueue, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnTasks(ctx, fromOwnTasksRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskQueue(v), nil
}
func (s *activityServer) OwnTask(ctx context.Context, r *pb.OwnTaskRequest) (*pb.TaskDetail, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnTask(ctx, fromOwnTaskRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskDetail(v), nil
}
func (s *activityServer) CompleteOwnTask(ctx context.Context, r *pb.CompleteTaskRequest) (*pb.TaskDetail, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.CompleteOwnTask(ctx, fromCompleteTaskRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskDetail(v), nil
}
func (s *activityServer) OwnLead(ctx context.Context, r *pb.OwnLeadRequest) (*pb.EntityContext, error) {
	tasks, err := employeeTasks(s.impl)
	if err != nil {
		return nil, err
	}
	v, err := tasks.OwnLead(ctx, fromOwnLeadRequest(r))
	if err != nil {
		return nil, err
	}
	return toEntityContext(v), nil
}

func (c activityClient) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	v, err := c.remote.OwnTasks(ctx, toOwnTasksRequest(r))
	if err != nil {
		return serviceapi.TaskQueue{}, err
	}
	return taskQueueFromPB(v), nil
}
func (c activityClient) OwnTask(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	v, err := c.remote.OwnTask(ctx, toOwnTaskRequest(r))
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return fromTaskDetail(v), nil
}
func (c activityClient) CompleteOwnTask(ctx context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	v, err := c.remote.CompleteOwnTask(ctx, toCompleteTaskRequest(r))
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return fromTaskDetail(v), nil
}
func (c activityClient) OwnLead(ctx context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	v, err := c.remote.OwnLead(ctx, toOwnLeadRequest(r))
	if err != nil {
		return serviceapi.EntityContext{}, err
	}
	return fromEntityContext(v), nil
}

var (
	_ serviceapi.EmployeeTasks = gatewayClient{}
	_ serviceapi.EmployeeTasks = activityClient{}
)
