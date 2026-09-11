package activity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Service) ResolveShare(ctx context.Context, req serviceapi.ShareLookupRequest) (serviceapi.ShareLookup, error) {
	key := trimViewKey(req.ViewKey)
	if key == "" || len(key) > 128 {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	hash := sha256.Sum256([]byte(key))
	lookup, err := s.store.ResolveShare(ctx, hash[:])
	if err != nil {
		if serviceapi.ErrorCode(err) == serviceapi.NotFound {
			return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
		}
		return serviceapi.ShareLookup{}, err
	}
	return lookup, nil
}

func (s *Service) CreatePanel(ctx context.Context, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	p, err := s.policy.Validate(ctx, command.Auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := serviceapi.ValidatePanelName(command.Name); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := serviceapi.ValidateEmployeeIDs(command.EmployeeIDs); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := serviceapi.ValidateDisplayWindow(command.DisplayWindow); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	directory, err := s.directoryFor(ctx, command.Auth, command.EmployeeIDs)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	enabled := true
	if command.Enabled != nil {
		enabled = *command.Enabled
	}
	key, hash, err := newViewKey()
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	panel := serviceapi.ManagedPanel{
		ID:            uuid.New(),
		Name:          command.Name,
		EmployeeIDs:   slices.Clone(command.EmployeeIDs),
		DisplayWindow: command.DisplayWindow,
		Timezone:      directory.Timezone,
		Enabled:       enabled,
		ViewKey:       key,
	}
	stored, err := s.store.CreatePanel(ctx, p, command, panel, hash[:])
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return s.withShareURL(stored), nil
}

func (s *Service) ListPanels(ctx context.Context, auth serviceapi.Auth) ([]serviceapi.ManagedPanel, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return nil, err
	}
	panels, err := s.store.ListPanels(ctx, p.Scope)
	if err != nil {
		return nil, err
	}
	for i := range panels {
		panels[i] = redactManaged(panels[i])
	}
	if panels == nil {
		panels = []serviceapi.ManagedPanel{}
	}
	return panels, nil
}

func (s *Service) GetManagedPanel(ctx context.Context, ref serviceapi.PanelRef) (serviceapi.ManagedPanel, error) {
	p, err := s.policy.Validate(ctx, ref.Auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	panel, err := s.store.GetPanel(ctx, p.Scope, ref.PanelID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return redactManaged(panel), nil
}

func (s *Service) PatchPanel(ctx context.Context, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	p, err := s.policy.Validate(ctx, command.Auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if command.Revision < 1 {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.InvalidArgument, "revision is required")
	}
	if command.HasName {
		if err := serviceapi.ValidatePanelName(command.Name); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
	}
	if command.HasEmployees {
		if err := serviceapi.ValidateEmployeeIDs(command.EmployeeIDs); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
		if _, err := s.directoryFor(ctx, command.Auth, command.EmployeeIDs); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
	}
	if command.HasWindow {
		if err := serviceapi.ValidateDisplayWindow(command.DisplayWindow); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
	}
	panel, err := s.store.PatchPanel(ctx, p, command)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return redactManaged(panel), nil
}

func (s *Service) RotateShareLink(ctx context.Context, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	p, err := s.policy.Validate(ctx, command.Auth, serviceapi.ActivityService, serviceapi.ActionPanels)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if command.PanelID == uuid.Nil {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if _, err := s.store.GetPanel(ctx, p.Scope, command.PanelID); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	key, hash, err := newViewKey()
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	panel, err := s.store.RotateShareLink(ctx, p, command, hash[:], key)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return s.withShareURL(panel), nil
}

func (s *Service) ListEmployees(ctx context.Context, auth serviceapi.Auth) (serviceapi.Directory, error) {
	if _, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionPanels); err != nil {
		return serviceapi.Directory{}, err
	}
	directory, err := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: auth})
	if err != nil {
		return serviceapi.Directory{}, err
	}
	if len(directory.Users) > 100 {
		return serviceapi.Directory{}, serviceapi.Fail(serviceapi.ResourceExhausted, "select at most 100 employees")
	}
	if directory.Users == nil {
		directory.Users = []serviceapi.User{}
	}
	return directory, nil
}

func (s *Service) ViewPanel(ctx context.Context, auth serviceapi.Auth) (serviceapi.ViewerPanel, error) {
	p, panel, users, err := s.viewerContext(ctx, auth, nil)
	if err != nil {
		return serviceapi.ViewerPanel{}, err
	}
	_ = p
	return serviceapi.ViewerPanel{
		Name:                  panel.Name,
		Timezone:              panel.Timezone,
		DisplayWindow:         panel.DisplayWindow,
		Employees:             users,
		InterpretationVersion: serviceapi.InterpretationVersion,
	}, nil
}

func (s *Service) ViewTimeline(ctx context.Context, query serviceapi.Query) (serviceapi.ViewerTimeline, error) {
	if err := serviceapi.ValidateQuery(query); err != nil {
		return serviceapi.ViewerTimeline{}, err
	}
	_, panel, users, err := s.viewerContext(ctx, query.Auth, query.UserIDs)
	if err != nil {
		return serviceapi.ViewerTimeline{}, err
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	data, err := s.viewerQuery(ctx, query, panel, users, true)
	if err != nil {
		return serviceapi.ViewerTimeline{}, err
	}
	result := serviceapi.ViewerTimeline{
		Employees:             users,
		Timezone:              panel.Timezone,
		Data:                  data,
		InterpretationVersion: serviceapi.InterpretationVersion,
	}
	result.Coverage, result.Freshness, result.EmptyReason = periodState(query, data)
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.ViewerTimeline{}, err
	}
	return result, nil
}

func (s *Service) ViewEmployee(ctx context.Context, query serviceapi.Query) (serviceapi.ViewerEmployee, error) {
	if err := serviceapi.ValidateQuery(query); err != nil {
		return serviceapi.ViewerEmployee{}, err
	}
	if len(query.UserIDs) != 1 {
		return serviceapi.ViewerEmployee{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	_, panel, users, err := s.viewerContext(ctx, query.Auth, query.UserIDs)
	if err != nil {
		return serviceapi.ViewerEmployee{}, err
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	data, err := s.viewerQuery(ctx, query, panel, users, false)
	if err != nil {
		return serviceapi.ViewerEmployee{}, err
	}
	result := serviceapi.ViewerEmployee{
		Employee:              users[0],
		Timezone:              panel.Timezone,
		Data:                  data,
		InterpretationVersion: serviceapi.InterpretationVersion,
	}
	result.Coverage, result.Freshness, result.EmptyReason = periodState(query, data)
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.ViewerEmployee{}, err
	}
	return result, nil
}

func (s *Service) ViewEvent(ctx context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	if err := serviceapi.ValidateEventRequest(req); err != nil {
		return serviceapi.Event{}, err
	}
	_, panel, users, err := s.viewerContext(ctx, req.Auth, nil)
	if err != nil {
		return serviceapi.Event{}, err
	}
	reader, ok := s.events.(serviceapi.EventReader)
	if !ok {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.Unavailable, "event detail reader unavailable")
	}
	event, err := reader.GetEvent(ctx, req)
	if err != nil {
		if serviceapi.ErrorCode(err) == serviceapi.NotFound {
			return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "not found")
		}
		return serviceapi.Event{}, err
	}
	if !containsID(panel.EmployeeIDs, event.CreatedBy) {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	event = presentEvent(event, users, true)
	if err := serviceapi.ValidateResponseSize(event); err != nil {
		return serviceapi.Event{}, err
	}
	return event, nil
}

func (s *Service) viewerContext(ctx context.Context, auth serviceapi.Auth, requested []int64) (serviceapi.Principal, serviceapi.ManagedPanel, []serviceapi.User, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionView)
	if err != nil {
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, err
	}
	if p.PanelID == uuid.Nil {
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	panel, err := s.store.GetPanel(ctx, p.Scope, p.PanelID)
	if err != nil {
		if serviceapi.ErrorCode(err) == serviceapi.NotFound {
			return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, serviceapi.Fail(serviceapi.NotFound, "not found")
		}
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, err
	}
	if !panel.Enabled || p.ViewKeyVersion < 1 || p.ViewKeyVersion != panel.ViewKeyVersion {
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if _, extra := extraIDs(requested, panel.EmployeeIDs); extra {
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	directory, err := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: auth, UserIDs: panel.EmployeeIDs})
	if err != nil {
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, err
	}
	users := projectEmployees(panel.EmployeeIDs, directory.Users)
	if len(requested) == 1 {
		for _, user := range users {
			if user.ID == requested[0] {
				return p, panel, []serviceapi.User{user}, nil
			}
		}
		return serviceapi.Principal{}, serviceapi.ManagedPanel{}, nil, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return p, panel, users, nil
}

func (s *Service) viewerQuery(ctx context.Context, query serviceapi.Query, panel serviceapi.ManagedPanel, users []serviceapi.User, timeline bool) (serviceapi.QueryResult, error) {
	resolved := query
	if len(resolved.UserIDs) == 0 {
		resolved.UserIDs = slices.Clone(panel.EmployeeIDs)
	}
	resolved.IncludeUnknownAuthors = false
	resolved.DirectoryUserIDs = nil
	resolved.GroupID = 0
	if timeline && (resolved.Buckets == "" || resolved.Buckets == serviceapi.BucketNone) {
		resolved.Buckets = serviceapi.BucketAuto
	}
	if resolved.Buckets != "" && resolved.Buckets != serviceapi.BucketNone {
		resolved.Timezone = panel.Timezone
	} else {
		resolved.Timezone = ""
	}
	data, err := s.events.Query(ctx, resolved)
	if err != nil {
		if query.Cursor != "" && serviceapi.ErrorCode(err) == serviceapi.InvalidArgument {
			return serviceapi.QueryResult{}, serviceapi.Fail(serviceapi.NotFound, "not found")
		}
		return serviceapi.QueryResult{}, err
	}
	if err := serviceapi.RequireQueryVersion(resolved, data); err != nil {
		return serviceapi.QueryResult{}, err
	}
	data.Events = presentEvents(data.Events, users)
	return data, nil
}

func (s *Service) directoryFor(ctx context.Context, auth serviceapi.Auth, ids []int64) (serviceapi.Directory, error) {
	directory, err := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: auth, UserIDs: ids})
	if err != nil {
		return serviceapi.Directory{}, err
	}
	if directory.Timezone == "" {
		return serviceapi.Directory{}, serviceapi.Fail(serviceapi.Unavailable, "account timezone is unavailable")
	}
	known := map[int64]struct{}{}
	for _, user := range directory.Users {
		known[user.ID] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			return serviceapi.Directory{}, serviceapi.Fail(serviceapi.InvalidArgument, "employee is not in this installation directory")
		}
	}
	return directory, nil
}

func (s *Service) withShareURL(panel serviceapi.ManagedPanel) serviceapi.ManagedPanel {
	panel.ShareUrlIssued = true
	if s.shareOrigin != "" && panel.ViewKey != "" {
		panel.ShareUrl = s.shareOrigin + "/#/p/" + panel.ViewKey
	}
	return panel
}

func redactManaged(panel serviceapi.ManagedPanel) serviceapi.ManagedPanel {
	panel.ViewKey = ""
	panel.ShareUrl = ""
	return panel
}

func newViewKey() (string, [32]byte, error) {
	var raw [serviceapi.ViewKeyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, serviceapi.Fail(serviceapi.Internal, "generate view key")
	}
	key := base64.RawURLEncoding.EncodeToString(raw[:])
	return key, sha256.Sum256([]byte(key)), nil
}

func trimViewKey(value string) string {
	return strings.TrimSpace(value)
}

func containsID(ids []int64, id int64) bool {
	return slices.Contains(ids, id)
}

func extraIDs(requested, allowed []int64) ([]int64, bool) {
	if len(requested) == 0 {
		return nil, false
	}
	allow := make(map[int64]struct{}, len(allowed))
	for _, id := range allowed {
		allow[id] = struct{}{}
	}
	var extra []int64
	for _, id := range requested {
		if _, ok := allow[id]; !ok {
			extra = append(extra, id)
		}
	}
	return extra, len(extra) > 0
}

func projectEmployees(ids []int64, directory []serviceapi.User) []serviceapi.User {
	byID := make(map[int64]serviceapi.User, len(directory))
	for _, user := range directory {
		byID[user.ID] = user
	}
	users := make([]serviceapi.User, 0, len(ids))
	for _, id := range ids {
		if user, ok := byID[id]; ok {
			users = append(users, user)
			continue
		}
		users = append(users, serviceapi.User{ID: id, Name: serviceapi.AuthorLabel(id, nil)})
	}
	return users
}
