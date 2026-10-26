package activity

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Service) ViewProblems(ctx context.Context, auth serviceapi.Auth) (serviceapi.ProblemSnapshot, error) {
	_, panel, _, err := s.viewerContext(ctx, auth, nil)
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	reader, ok := s.gateway.(serviceapi.ProblemReader)
	if !ok {
		return serviceapi.ProblemSnapshot{}, serviceapi.Fail(serviceapi.Unavailable, "current snapshot reader unavailable")
	}
	return reader.Problems(ctx, serviceapi.ProblemRequest{Auth: auth, EmployeeIDs: panel.EmployeeIDs, PipelineIDs: panel.Options.PipelineIDs})
}
