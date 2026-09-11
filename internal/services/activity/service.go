// Package activity owns only product settings and the presentation of CRM
// observations. CRM Events is the sole owner of event history and collection.
package activity

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type Repository interface {
	Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error)
	Configure(context.Context, serviceapi.Principal, serviceapi.SettingsCommand) (serviceapi.Operation, error)
	Operation(context.Context, serviceapi.Principal, string) (serviceapi.Operation, error)
	ResolveShare(context.Context, []byte) (serviceapi.ShareLookup, error)
	CreatePanel(context.Context, serviceapi.Principal, serviceapi.PanelCommand, serviceapi.ManagedPanel, []byte) (serviceapi.ManagedPanel, error)
	ListPanels(context.Context, serviceapi.Scope) ([]serviceapi.ManagedPanel, error)
	GetPanel(context.Context, serviceapi.Scope, uuid.UUID) (serviceapi.ManagedPanel, error)
	PatchPanel(context.Context, serviceapi.Principal, serviceapi.PanelCommand) (serviceapi.ManagedPanel, error)
	RotateShareLink(context.Context, serviceapi.Principal, serviceapi.PanelCommand, []byte, string) (serviceapi.ManagedPanel, error)
}

type Service struct {
	store       Repository
	policy      serviceapi.Policy
	events      serviceapi.CRMEvents
	gateway     serviceapi.Gateway
	shareOrigin string
}

func New(store Repository, policy serviceapi.Policy, events serviceapi.CRMEvents, gateway serviceapi.Gateway) *Service {
	return &Service{store: store, policy: policy, events: events, gateway: gateway}
}

func (s *Service) WithShareOrigin(origin string) *Service {
	s.shareOrigin = strings.TrimRight(strings.TrimSpace(origin), "/")
	return s
}

var _ serviceapi.Activity = (*Service)(nil)
var _ serviceapi.EventPresenter = (*Service)(nil)

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
	directoryIDs := query.UserIDs
	if query.IncludeUnknownAuthors && query.GroupID == 0 {
		// A selected subset cannot certify which other authors are unknown.
		// Keep the existing bounded directory contract; fail if it is too large.
		directoryIDs = nil
	}
	directory, err := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: query.Auth, UserIDs: directoryIDs})
	if err != nil {
		return serviceapi.Panel{}, err
	}
	if len(directory.Users) > 100 {
		return serviceapi.Panel{}, serviceapi.Fail(serviceapi.ResourceExhausted, "select at most 100 employees")
	}
	users, resolved, empty := resolveEmployees(query, directory, p.ActorID)
	if empty {
		settings, err := s.store.Settings(ctx, p.Scope)
		if err != nil {
			return serviceapi.Panel{}, err
		}
		status, err := s.events.Status(ctx, query.Auth)
		if err != nil {
			return serviceapi.Panel{}, err
		}
		data := serviceapi.QueryResult{Events: []serviceapi.Event{}, Summaries: []serviceapi.UserSummary{}, ReadVersion: serviceapi.PresentationReadVersion, Status: status}
		result := serviceapi.Panel{Users: users, Timezone: directory.Timezone, Data: data, Settings: settings, InterpretationVersion: serviceapi.InterpretationVersion}
		result.Coverage, result.Freshness, result.EmptyReason = periodState(query, data)
		return result, serviceapi.ValidateResponseSize(result)
	}
	data, err := s.events.Query(ctx, resolved)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	if err := serviceapi.RequireQueryVersion(resolved, data); err != nil {
		return serviceapi.Panel{}, err
	}
	settings, err := s.store.Settings(ctx, p.Scope)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	users = appendUnknownAuthors(users, data.Summaries)
	data.Events = presentEvents(data.Events, users)
	result := serviceapi.Panel{Users: users, Timezone: directory.Timezone, Data: data, Settings: settings, InterpretationVersion: serviceapi.InterpretationVersion}
	result.Coverage, result.Freshness, result.EmptyReason = periodState(query, data)
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.Panel{}, err
	}
	return result, nil
}

func (s *Service) EventCard(ctx context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	if _, err := s.policy.Validate(ctx, req.Auth, serviceapi.ActivityService, serviceapi.ActionPanel); err != nil {
		return serviceapi.Event{}, err
	}
	if err := serviceapi.ValidateEventRequest(req); err != nil {
		return serviceapi.Event{}, err
	}
	reader, ok := s.events.(serviceapi.EventReader)
	if !ok {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.Unavailable, "event detail reader unavailable")
	}
	event, err := reader.GetEvent(ctx, req)
	if err != nil {
		return serviceapi.Event{}, err
	}
	var users []serviceapi.User
	if event.CreatedBy > 0 {
		directory, dirErr := s.gateway.Users(ctx, serviceapi.UsersRequest{Auth: req.Auth, UserIDs: []int64{event.CreatedBy}})
		if dirErr == nil {
			users = directory.Users
		}
	}
	event = presentEvent(event, users, true)
	if err := serviceapi.ValidateResponseSize(event); err != nil {
		return serviceapi.Event{}, err
	}
	return event, nil
}

func resolveEmployees(query serviceapi.Query, directory serviceapi.Directory, actor int64) ([]serviceapi.User, serviceapi.Query, bool) {
	known := make([]int64, 0, len(directory.Users))
	for _, user := range directory.Users {
		known = append(known, user.ID)
	}
	users := append([]serviceapi.User{}, directory.Users...)
	if query.GroupID > 0 {
		query.IncludeUnknownAuthors = false
		var filtered []serviceapi.User
		for _, user := range users {
			if user.GroupID == query.GroupID {
				filtered = append(filtered, user)
			}
		}
		users = filtered
	}
	explicit := len(query.UserIDs) > 0
	if explicit {
		allowed := map[int64]bool{}
		for _, user := range users {
			allowed[user.ID] = true
		}
		var selected []int64
		var selectedUsers []serviceapi.User
		seen := map[int64]bool{}
		for _, id := range query.UserIDs {
			if (query.GroupID > 0 && !allowed[id]) || seen[id] {
				continue
			}
			seen[id] = true
			selected = append(selected, id)
			for _, user := range users {
				if user.ID == id {
					selectedUsers = append(selectedUsers, user)
				}
			}
		}
		users = selectedUsers
		query.UserIDs = selected
		if len(query.UserIDs) == 0 {
			return users, query, true
		}
	}
	if len(query.UserIDs) == 0 {
		for _, user := range users {
			query.UserIDs = append(query.UserIDs, user.ID)
		}
		if len(query.UserIDs) == 0 {
			if query.GroupID > 0 {
				return users, query, true
			}
			query.UserIDs = []int64{actor}
		} else if query.GroupID == 0 {
			query.IncludeUnknownAuthors = true
		}
	}
	if query.IncludeUnknownAuthors {
		query.DirectoryUserIDs = known
	} else {
		query.DirectoryUserIDs = nil
	}
	query.GroupID = 0
	if query.Buckets != "" && query.Buckets != serviceapi.BucketNone {
		query.Timezone = directory.Timezone
	} else {
		query.Timezone = ""
	}
	return users, query, false
}

func appendUnknownAuthors(users []serviceapi.User, summaries []serviceapi.UserSummary) []serviceapi.User {
	known := map[int64]bool{}
	for _, user := range users {
		known[user.ID] = true
	}
	for _, summary := range summaries {
		if known[summary.UserID] {
			continue
		}
		users = append(users, serviceapi.User{ID: summary.UserID, Name: serviceapi.AuthorLabel(summary.UserID, nil)})
		known[summary.UserID] = true
	}
	return users
}

// periodState splits selected-range coverage from collector freshness.
// Lag on a later window does not mark an already verified completed range incomplete.
func periodState(query serviceapi.Query, data serviceapi.QueryResult) (coverage, freshness, empty string) {
	status := data.Status
	switch {
	case status.VerifiedThrough == 0 || status.VerifiedFrom == 0:
		coverage = serviceapi.CoverageUnknown
	case query.To < status.VerifiedFrom || query.To < status.HistoryFrom:
		coverage = serviceapi.CoverageUnknown
	case query.From < status.VerifiedFrom || query.From < status.HistoryFrom || query.To > status.VerifiedThrough:
		coverage = serviceapi.CoveragePartial
	default:
		coverage = serviceapi.CoverageVerified
	}
	switch {
	case status.ReauthRequired:
		freshness = serviceapi.FreshnessReauthRequired
	case status.ErrorCode != "":
		freshness = serviceapi.FreshnessError
	case status.LagSeconds > 600:
		freshness = serviceapi.FreshnessLagging
	default:
		freshness = serviceapi.FreshnessCurrent
	}
	count := data.Totals.UniqueEvents
	if count == 0 {
		for _, summary := range data.Summaries {
			count += summary.UniqueEvents
		}
	}
	if count == 0 {
		if coverage == serviceapi.CoverageVerified {
			empty = serviceapi.EmptyReasonNoEvents
		} else {
			empty = serviceapi.EmptyReasonUnverified
		}
	}
	return coverage, freshness, empty
}
