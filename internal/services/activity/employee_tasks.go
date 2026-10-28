package activity

import (
	"context"
	"slices"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Activity owns the task panel scenario: who is enrolled and whether related
// deals are shown. Gateway owns every amoCRM read and write.
func (s *Service) taskGateway(ctx context.Context, auth serviceapi.Auth, requireEnabled bool) (serviceapi.EmployeeTasks, serviceapi.TaskPanelSettings, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionSessions)
	if err != nil {
		return nil, serviceapi.TaskPanelSettings{}, err
	}
	if p.Kind != serviceapi.PrincipalKindEmployee || p.ActorID <= 0 {
		return nil, serviceapi.TaskPanelSettings{}, serviceapi.Fail(serviceapi.PermissionDenied, "employee identity required")
	}
	tasks, ok := s.gateway.(serviceapi.EmployeeTasks)
	if !ok {
		return nil, serviceapi.TaskPanelSettings{}, serviceapi.Fail(serviceapi.Unavailable, "employee task gateway unavailable")
	}
	store, err := s.sessionStore()
	if err != nil {
		return nil, serviceapi.TaskPanelSettings{}, err
	}
	settings, err := store.TaskPanel(ctx, p.Scope)
	if err != nil {
		return nil, serviceapi.TaskPanelSettings{}, err
	}
	if requireEnabled && !(settings.Enabled && slices.Contains(settings.EmployeeIDs, p.ActorID)) {
		return nil, serviceapi.TaskPanelSettings{}, serviceapi.Fail(serviceapi.PermissionDenied, "task panel is disabled for this employee")
	}
	return tasks, settings, nil
}

func (s *Service) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	tasks, settings, err := s.taskGateway(ctx, r.Auth, true)
	if err != nil {
		return serviceapi.TaskQueue{}, err
	}
	if !settings.IncludeRelatedDeals {
		r.LeadID = 0
	}
	return tasks.OwnTasks(ctx, r)
}

// OwnTask stays readable for a disabled employee: an interval started before
// the panel was turned off must still be reconciled with amoCRM.
func (s *Service) OwnTask(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	tasks, _, err := s.taskGateway(ctx, r.Auth, false)
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return tasks.OwnTask(ctx, r)
}

func (s *Service) CompleteOwnTask(ctx context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	tasks, _, err := s.taskGateway(ctx, r.Auth, true)
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	return tasks.CompleteOwnTask(ctx, r)
}

func (s *Service) OwnLead(ctx context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	tasks, _, err := s.taskGateway(ctx, r.Auth, true)
	if err != nil {
		return serviceapi.EntityContext{}, err
	}
	return tasks.OwnLead(ctx, r)
}

var _ serviceapi.EmployeeTasks = (*Service)(nil)
