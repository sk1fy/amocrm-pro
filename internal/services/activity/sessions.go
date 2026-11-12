package activity

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type sessionRepository interface {
	CurrentSession(context.Context, serviceapi.Scope, int64) (serviceapi.WorkSession, error)
	SessionReceipt(context.Context, serviceapi.Principal, serviceapi.SessionCommand) (serviceapi.WorkSession, bool, error)
	ApplySession(context.Context, serviceapi.Principal, serviceapi.SessionCommand, serviceapi.SessionContext) (serviceapi.WorkSession, error)
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
	if c.Action != serviceapi.SessionActionStartTask {
		return store.ApplySession(ctx, p, c, c.Context)
	}
	if receipt, found, err := store.SessionReceipt(ctx, p, c); found || err != nil {
		return receipt, err
	}
	resolved, err := s.taskSessionContext(ctx, store, p, c)
	if err != nil {
		if receipt, found, receiptErr := store.SessionReceipt(ctx, p, c); found || receiptErr != nil {
			return receipt, receiptErr
		}
		return serviceapi.WorkSession{}, err
	}
	return store.ApplySession(ctx, p, c, resolved)
}

func (s *Service) taskSessionContext(ctx context.Context, store sessionRepository, p serviceapi.Principal, c serviceapi.SessionCommand) (serviceapi.SessionContext, error) {
	settings, err := store.TaskPanel(ctx, p.Scope)
	if err != nil {
		return serviceapi.SessionContext{}, err
	}
	if !taskPanelEnabled(settings, p.ActorID) {
		return serviceapi.SessionContext{}, serviceapi.Fail(serviceapi.PermissionDenied, "task panel is disabled for employee")
	}
	tasks, ok := s.gateway.(serviceapi.EmployeeTasks)
	if !ok {
		return serviceapi.SessionContext{}, serviceapi.Fail(serviceapi.Unavailable, "employee task gateway unavailable")
	}
	detail, err := tasks.OwnTask(ctx, serviceapi.OwnTaskRequest{Auth: c.Auth, TaskID: c.Context.TaskID, WithContext: true})
	if err != nil {
		return serviceapi.SessionContext{}, err
	}
	if detail.ID != c.Context.TaskID {
		return serviceapi.SessionContext{}, serviceapi.Fail(serviceapi.Unavailable, "task gateway returned an unexpected task")
	}
	if detail.IsCompleted {
		return serviceapi.SessionContext{}, serviceapi.Fail(serviceapi.Conflict, "task is already completed")
	}
	result := serviceapi.SessionContext{
		ClientID:     c.Context.ClientID,
		TaskID:       detail.ID,
		TaskName:     sessionText(detail.Text, serviceapi.SessionContextTextLimit),
		TaskTypeID:   detail.TaskTypeID,
		TaskTypeName: sessionText(detail.TaskTypeName, serviceapi.SessionContextTextLimit),
		EntityID:     detail.EntityID,
		EntityType:   detail.EntityType,
	}
	if entity := detail.Entity; !entity.Restricted && entity.ID > 0 && entity.ID == detail.EntityID && entity.EntityType == detail.EntityType {
		result.EntityName = sessionText(entity.Name, serviceapi.SessionContextTextLimit)
		result.PipelineID = entity.PipelineID
		result.PipelineName = sessionText(entity.PipelineName, serviceapi.SessionContextTextLimit)
		result.StatusID = entity.StatusID
		result.StatusName = sessionText(entity.StatusName, serviceapi.SessionContextTextLimit)
		result.StatusColor = sessionText(entity.StatusColor, 16)
	}
	return result, nil
}

func sessionText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\x00", "") // PostgreSQL jsonb cannot store NUL.
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func taskPanelEnabled(settings serviceapi.TaskPanelSettings, employeeID int64) bool {
	return settings.Enabled && slices.Contains(settings.EmployeeIDs, employeeID)
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
	enabled := taskPanelEnabled(settings, p.ActorID)
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
