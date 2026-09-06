package crmevents

import (
	"context"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Service) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	p, err := s.authorize(ctx, q.Auth, serviceapi.ActionRead)
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	if err = serviceapi.ValidateQuery(q); err != nil {
		return serviceapi.QueryResult{}, err
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	result, err := s.repository.Query(ctx, q, p)
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	if err = serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.QueryResult{}, err
	}
	return result, nil
}
func (s *Service) Status(ctx context.Context, auth serviceapi.Auth) (serviceapi.SyncStatus, error) {
	p, err := s.authorize(ctx, auth, serviceapi.ActionStatus)
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	return s.repository.Status(ctx, p)
}
