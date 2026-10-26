package gateway

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Service) Problems(ctx context.Context, r serviceapi.ProblemRequest) (serviceapi.ProblemSnapshot, error) {
	if err := serviceapi.ValidateEmployeeIDs(r.EmployeeIDs); err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	if err := serviceapi.ValidatePanelOptions(serviceapi.PanelOptions{PipelineIDs: r.PipelineIDs}, r.EmployeeIDs); err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionProblems)
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	if s.problemStore == nil {
		return serviceapi.ProblemSnapshot{}, serviceapi.Fail(serviceapi.Unavailable, "current snapshot collector unavailable")
	}
	if err := s.problemStore.Request(ctx, p.Scope); err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	return s.problemStore.Read(ctx, p.Scope, r)
}
