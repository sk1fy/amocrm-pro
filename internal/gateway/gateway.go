// Package gateway exposes only bounded events and directory reads through the
// existing Core-owned amoCRM client. No new token storage or rate limiter exists.
package gateway

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"sync"
	"time"
)

type API interface {
	ListEvents(context.Context, uuid.UUID, int64, int64, int, int) (amocrm.CRMEventPage, error)
	GetDirectory(context.Context, uuid.UUID) (amocrm.AccountDirectory, error)
}
type cached struct {
	data  amocrm.AccountDirectory
	until time.Time
}
type Service struct {
	api       API
	policy    serviceapi.Policy
	mu        sync.Mutex
	directory map[uuid.UUID]cached
}

func New(api API, policy serviceapi.Policy) *Service {
	return &Service{api: api, policy: policy, directory: map[uuid.UUID]cached{}}
}
func (s *Service) Events(ctx context.Context, r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	if r.From < 0 || r.To < r.From || r.To-r.From > 31*86400 || r.Page < 1 || r.Page > 100000 || r.Limit < 1 || r.Limit > 100 {
		return serviceapi.EventPage{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid event window or page")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionEvents)
	if err != nil {
		return serviceapi.EventPage{}, err
	}
	page, err := s.api.ListEvents(ctx, p.InstallationID, r.From, r.To, r.Page, r.Limit)
	if err != nil {
		return serviceapi.EventPage{}, corepolicy.MapUpstreamError(err)
	}
	events := make([]serviceapi.Event, 0, len(page.Events))
	for _, e := range page.Events {
		events = append(events, serviceapi.Event{ID: e.ID, CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy, Type: e.Type, EntityID: e.EntityID, EntityType: e.EntityType, ValueBefore: e.ValueBefore, ValueAfter: e.ValueAfter})
	}
	return serviceapi.EventPage{Events: events, HasNext: page.HasNext}, nil
}
func (s *Service) Users(ctx context.Context, r serviceapi.UsersRequest) (serviceapi.Directory, error) {
	if len(r.UserIDs) > 100 {
		return serviceapi.Directory{}, serviceapi.Fail(serviceapi.InvalidArgument, "at most 100 requested users")
	}
	selected := map[int64]bool{}
	for _, id := range r.UserIDs {
		if id <= 0 {
			return serviceapi.Directory{}, serviceapi.Fail(serviceapi.InvalidArgument, "user id must be positive")
		}
		selected[id] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionUsers)
	if err != nil {
		return serviceapi.Directory{}, err
	}
	s.mu.Lock()
	entry, ok := s.directory[p.InstallationID]
	s.mu.Unlock()
	if !ok || time.Now().After(entry.until) {
		d, err := s.api.GetDirectory(ctx, p.InstallationID)
		if err != nil {
			return serviceapi.Directory{}, corepolicy.MapUpstreamError(err)
		}
		if err := validateDirectory(d); err != nil {
			return serviceapi.Directory{}, err
		}
		entry = cached{data: d, until: time.Now().Add(30 * time.Second)}
		s.mu.Lock()
		if len(s.directory) >= 256 {
			for id, v := range s.directory {
				if time.Now().After(v.until) {
					delete(s.directory, id)
				}
			}
		}
		if len(s.directory) < 256 {
			s.directory[p.InstallationID] = entry
		}
		s.mu.Unlock()
	}
	result := serviceapi.Directory{Timezone: entry.data.Timezone, Users: []serviceapi.User{}}
	for _, u := range entry.data.Users {
		if len(selected) == 0 || selected[u.ID] {
			result.Users = append(result.Users, serviceapi.User{ID: u.ID, Name: u.Name, GroupID: u.GroupID, GroupName: u.GroupName})
		}
	}
	if len(result.Users) > 100 {
		return serviceapi.Directory{}, serviceapi.Fail(serviceapi.InvalidArgument, "choose at most 100 employees for panel")
	}
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.Directory{}, err
	}
	return result, nil
}

// Cache only bounded display metadata; authorization is still checked live.
// This cap applies before insertion, including to filtered directory requests.
const maxCachedDirectoryBytes = 512 << 10

func validateDirectory(directory amocrm.AccountDirectory) error {
	if len(directory.Users) > 1000 || len(directory.Timezone) > 128 {
		return serviceapi.Fail(serviceapi.ResourceExhausted, "employee directory exceeds v0 size bounds")
	}
	for _, user := range directory.Users {
		if len(user.Name) > 512 || len(user.GroupName) > 256 {
			return serviceapi.Fail(serviceapi.ResourceExhausted, "employee directory exceeds v0 field bounds")
		}
	}
	encoded, err := json.Marshal(directory)
	if err != nil {
		return serviceapi.Fail(serviceapi.Internal, "invalid employee directory")
	}
	if len(encoded) > maxCachedDirectoryBytes {
		return serviceapi.Fail(serviceapi.ResourceExhausted, "employee directory exceeds v0 cache size limit")
	}
	return nil
}
