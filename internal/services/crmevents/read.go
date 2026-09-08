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

// GetEvent reads the owner store under live policy, without fetching amoCRM or
// turning a missing, retained-away, or foreign ID into a different public error.
func (s *Service) GetEvent(ctx context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	p, err := s.authorize(ctx, req.Auth, serviceapi.ActionRead)
	if err != nil {
		return serviceapi.Event{}, err
	}
	if err = serviceapi.ValidateEventRequest(req); err != nil {
		return serviceapi.Event{}, err
	}
	event, err := s.repository.GetEvent(ctx, p, req.EventID)
	if err != nil {
		return serviceapi.Event{}, err
	}
	if loader, ok := s.repository.(interface {
		LoadEventEnrichment(context.Context, serviceapi.Principal, serviceapi.Event) (serviceapi.Event, error)
	}); ok {
		event, err = loader.LoadEventEnrichment(ctx, p, event)
		if err != nil {
			return serviceapi.Event{}, err
		}
	}
	if err = serviceapi.ValidateResponseSize(event); err != nil {
		return serviceapi.Event{}, err
	}
	return event, nil
}
