package activity

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"slices"
)

type sessionRepository interface {
	CurrentSession(context.Context, serviceapi.Scope, int64) (serviceapi.WorkSession, error)
	ApplySession(context.Context, serviceapi.Principal, serviceapi.SessionCommand) (serviceapi.WorkSession, error)
	ReadSessions(context.Context, serviceapi.Scope, int64, int64, int64, string) (serviceapi.SessionPage, error)
	TaskPanel(context.Context, serviceapi.Scope) (serviceapi.TaskPanelSettings, error)
	SaveTaskPanel(context.Context, serviceapi.Scope, serviceapi.TaskPanelSettings) (serviceapi.TaskPanelSettings, error)
}

func (s *Service) sessionStore() (sessionRepository, error) {
	r, ok := s.store.(sessionRepository)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "session storage unavailable")
	}
	return r, nil
}
func (s *Service) ApplySession(ctx context.Context, c serviceapi.SessionCommand) (serviceapi.WorkSession, error) {
	p, err := s.policy.Validate(ctx, c.Auth, serviceapi.ActivityService, serviceapi.ActionSessions)
	if err != nil {
		return serviceapi.WorkSession{}, err
	}
	if p.Kind != serviceapi.PrincipalKindEmployee || p.ActorID <= 0 {
		return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.PermissionDenied, "employee identity required")
	}
	if err := serviceapi.ValidateSessionCommand(c); err != nil {
		return serviceapi.WorkSession{}, err
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.WorkSession{}, err
	}
	return store.ApplySession(ctx, p, c)
}
func (s *Service) ViewSessions(ctx context.Context, q serviceapi.Query) (serviceapi.SessionPage, error) {
	if err := serviceapi.ValidateQuery(q); err != nil {
		return serviceapi.SessionPage{}, err
	}
	if len(q.UserIDs) != 1 {
		return serviceapi.SessionPage{}, serviceapi.Fail(serviceapi.InvalidArgument, "one employee required")
	}
	p, _, _, err := s.viewerContext(ctx, q.Auth, q.UserIDs)
	if err != nil {
		return serviceapi.SessionPage{}, err
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.SessionPage{}, err
	}
	return store.ReadSessions(ctx, p.Scope, q.UserIDs[0], q.From, q.To, q.Cursor)
}
func (s *Service) TaskPanel(ctx context.Context, auth serviceapi.Auth) (serviceapi.TaskPanelSettings, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	return store.TaskPanel(ctx, p.Scope)
}
func (s *Service) SaveTaskPanel(ctx context.Context, c serviceapi.TaskPanelCommand) (serviceapi.TaskPanelSettings, error) {
	p, err := s.policy.Validate(ctx, c.Auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	if err := serviceapi.ValidateTaskPanel(c.Settings); err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	// Validate enabled employees against the current installation directory in bounded batches.
	for i := 0; i < len(c.Settings.EmployeeIDs); i += 100 {
		end := min(i+100, len(c.Settings.EmployeeIDs))
		if _, err := s.directoryFor(ctx, c.Auth, c.Settings.EmployeeIDs[i:end]); err != nil {
			return serviceapi.TaskPanelSettings{}, err
		}
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	return store.SaveTaskPanel(ctx, p.Scope, c.Settings)
}

func (s *Service) OwnActivityState(ctx context.Context, auth serviceapi.Auth) (serviceapi.EmployeeActivityState, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionSessions)
	if err != nil {
		return serviceapi.EmployeeActivityState{}, err
	}
	if p.Kind != serviceapi.PrincipalKindEmployee || p.ActorID <= 0 {
		return serviceapi.EmployeeActivityState{}, serviceapi.Fail(serviceapi.PermissionDenied, "employee identity required")
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.EmployeeActivityState{}, err
	}
	settings, err := store.TaskPanel(ctx, p.Scope)
	if err != nil {
		return serviceapi.EmployeeActivityState{}, err
	}
	enabled := settings.Enabled && slices.Contains(settings.EmployeeIDs, p.ActorID)
	settings.EmployeeIDs = []int64{}
	if enabled {
		settings.EmployeeIDs = []int64{p.ActorID}
	}
	current, err := store.CurrentSession(ctx, p.Scope, p.ActorID)
	if err != nil {
		return serviceapi.EmployeeActivityState{}, err
	}
	return serviceapi.EmployeeActivityState{Current: current, Settings: settings, Enabled: enabled}, nil
}
func (s *Service) OwnSessions(ctx context.Context, q serviceapi.Query) (serviceapi.SessionPage, error) {
	if err := serviceapi.ValidateQuery(q); err != nil {
		return serviceapi.SessionPage{}, err
	}
	p, err := s.policy.Validate(ctx, q.Auth, serviceapi.ActivityService, serviceapi.ActionSessions)
	if err != nil {
		return serviceapi.SessionPage{}, err
	}
	if p.Kind != serviceapi.PrincipalKindEmployee || p.ActorID <= 0 {
		return serviceapi.SessionPage{}, serviceapi.Fail(serviceapi.PermissionDenied, "employee identity required")
	}
	store, err := s.sessionStore()
	if err != nil {
		return serviceapi.SessionPage{}, err
	}
	return store.ReadSessions(ctx, p.Scope, p.ActorID, q.From, q.To, q.Cursor)
}
