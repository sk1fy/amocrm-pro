// Package activity owns only product settings and the presentation of CRM
// observations. CRM Events is the sole owner of event history and collection.
package activity

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type Repository interface {
	Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error)
	Configure(context.Context, serviceapi.Principal, serviceapi.SettingsCommand) (serviceapi.Operation, error)
	Operation(context.Context, serviceapi.Principal, string) (serviceapi.Operation, error)
}

type Service struct {
	store   Repository
	policy  serviceapi.Policy
	events  serviceapi.CRMEvents
	gateway serviceapi.Gateway
}

func New(store Repository, policy serviceapi.Policy, events serviceapi.CRMEvents, gateway serviceapi.Gateway) *Service {
	return &Service{store: store, policy: policy, events: events, gateway: gateway}
}

func Defaults() serviceapi.Settings { return serviceapi.DefaultSettings() }

func ValidateSettings(s serviceapi.Settings) error {
	return serviceapi.ValidateSettings(s)
}

func (s *Service) Settings(ctx context.Context, auth serviceapi.Auth) (serviceapi.Settings, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return serviceapi.Settings{}, err
	}
	return s.store.Settings(ctx, p.Scope)
}

func (s *Service) Configure(ctx context.Context, command serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	p, err := s.policy.Validate(ctx, command.Auth, serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	if err := ValidateSettings(command.Settings); err != nil {
		return serviceapi.Operation{}, err
	}
	return s.store.Configure(ctx, p, command)
}

func (s *Service) Operation(ctx context.Context, request serviceapi.OperationRequest) (serviceapi.Operation, error) {
	p, err := s.policy.Validate(ctx, request.Auth, serviceapi.ActivityService, serviceapi.ActionOperation)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return s.store.Operation(ctx, p, request.OperationID)
}

func (s *Service) Panel(ctx context.Context, query serviceapi.Query) (serviceapi.Panel, error) {
	p, err := s.policy.Validate(ctx, query.Auth, serviceapi.ActivityService, serviceapi.ActionPanel)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	if err := serviceapi.ValidateQuery(query); err != nil {
		return serviceapi.Panel{}, err
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	// Resolve a bounded employee batch once, then pass exactly that batch to
	// the event owner. No per-employee RPC and no history fetch from amoCRM.
	directory, err := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: query.Auth, UserIDs: query.UserIDs})
	if err != nil {
		return serviceapi.Panel{}, err
	}
	if len(directory.Users) > 100 {
		return serviceapi.Panel{}, serviceapi.Fail(serviceapi.ResourceExhausted, "select at most 100 employees")
	}
	if len(query.UserIDs) == 0 {
		for _, user := range directory.Users {
			query.UserIDs = append(query.UserIDs, user.ID)
		}
		if len(query.UserIDs) == 0 {
			query.UserIDs = []int64{p.ActorID}
		}
	}
	data, err := s.events.Query(ctx, query)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	settings, err := s.store.Settings(ctx, p.Scope)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	result := serviceapi.Panel{Users: directory.Users, Timezone: directory.Timezone, Data: data, Settings: settings, Coverage: coverage(query, data.Status)}
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.Panel{}, err
	}
	return result, nil
}

// Coverage describes observations, never employee inactivity. A zero registered
// count is interpretable only together with the verified and retained bounds.
func coverage(query serviceapi.Query, status serviceapi.SyncStatus) string {
	if status.VerifiedThrough == 0 || status.VerifiedFrom == 0 {
		return "unknown"
	}
	if query.From < status.VerifiedFrom || query.From < status.HistoryFrom || query.To > status.VerifiedThrough {
		return "partial"
	}
	if status.ReauthRequired || status.ErrorCode != "" || status.LagSeconds > 600 {
		return "stale"
	}
	return "verified"
}
