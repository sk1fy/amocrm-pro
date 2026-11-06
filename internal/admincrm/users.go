// Package admincrm serves bounded admin reads of amoCRM account facts from the
// worker process, the only owner of outgoing amoCRM traffic (ADR-0019/0029).
// Reads share the worker OAuth provider and outbound budget; results are kept
// in a short process-local cache and never persisted (ADR-0042).
package admincrm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
)

// Read states. "ok" carries users; every other state carries none and means
// the list is not known, never that the account has no users.
const (
	StateOK                     = "ok"
	StateNoAuthorizedConnection = "no_authorized_connection"
	StateAuthError              = "auth_error"
	StateRateLimited            = "rate_limited"
	StateIncomplete             = "incomplete"
	StateNetworkError           = "network_error"
	StateInternalError          = "internal_error"
)

const (
	// FreshFor matches the crmreferences snapshot lifetime used for TeamOS.
	FreshFor        = 5 * time.Minute
	fetchTimeout    = 30 * time.Second
	maxCacheEntries = 256
)

var (
	ErrAccountNotFound        = errors.New("account not found")
	ErrNoAuthorizedConnection = errors.New("account has no authorized connection")
)

// Connection is the installation whose OAuth credentials read the account.
type Connection struct {
	InstallationID  uuid.UUID
	IntegrationID   uuid.UUID
	IntegrationCode string
}

// User is the admin projection of an amoCRM user. Contact fields, group, role
// and per-entity rights are deliberately absent.
type User struct {
	ID       int64
	Name     string
	IsActive bool
	IsAdmin  bool
}

type Snapshot struct {
	Connection Connection
	FetchedAt  time.Time
	FreshUntil time.Time
	Users      []User
}

// ReadError keeps the classified state of a failed amoCRM read and the
// connection that attempted it. The wrapped error is for logs only.
type ReadError struct {
	State      string
	RetryAfter time.Duration
	Connection Connection
	Err        error
}

func (e *ReadError) Error() string { return "read amoCRM users: " + e.State }
func (e *ReadError) Unwrap() error { return e.Err }

type Resolver interface {
	ResolveConnection(ctx context.Context, accountID int64) (Connection, error)
}

type Reader interface {
	CRMUsers(ctx context.Context, installationID uuid.UUID) ([]amocrm.CRMUser, error)
}

type Service struct {
	Resolver Resolver
	CRM      Reader
	Now      func() time.Time

	mu      sync.Mutex
	cache   map[int64]Snapshot
	flights singleflight.Group
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Users resolves the reading connection on every call, so a revoked or
// reauthorized installation never serves a cached list, then returns a cached
// snapshot younger than FreshFor or reads amoCRM once per account.
func (s *Service) Users(ctx context.Context, accountID int64) (Snapshot, error) {
	connection, err := s.Resolver.ResolveConnection(ctx, accountID)
	if err != nil {
		return Snapshot{}, err
	}
	if cached, ok := s.cached(accountID, connection); ok {
		return cached, nil
	}
	key := fmt.Sprintf("%d/%s", accountID, connection.InstallationID)
	result := s.flights.DoChan(key, func() (any, error) {
		if cached, ok := s.cached(accountID, connection); ok {
			return cached, nil
		}
		// One caller leaving must not fail the others waiting on this read.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		started := s.now()
		raw, err := s.CRM.CRMUsers(fetchCtx, connection.InstallationID)
		if err != nil {
			state, retryAfter := Classify(err)
			return Snapshot{}, &ReadError{State: state, RetryAfter: retryAfter, Connection: connection, Err: err}
		}
		users := make([]User, 0, len(raw))
		for _, item := range raw {
			active := item.Rights.IsActive != nil && *item.Rights.IsActive
			users = append(users, User{ID: item.ID, Name: item.Name, IsActive: active, IsAdmin: item.Rights.IsAdmin})
		}
		sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
		snapshot := Snapshot{Connection: connection, FetchedAt: s.now(), FreshUntil: started.Add(FreshFor), Users: users}
		s.store(accountID, snapshot)
		return snapshot, nil
	})
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return Snapshot{}, r.Err
		}
		snapshot, _ := r.Val.(Snapshot)
		return clone(snapshot), nil
	}
}

func (s *Service) cached(accountID int64, connection Connection) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.cache[accountID]
	if !ok || snapshot.Connection != connection || !s.now().Before(snapshot.FreshUntil) {
		return Snapshot{}, false
	}
	return clone(snapshot), true
}

func (s *Service) store(accountID int64, snapshot Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[int64]Snapshot{}
	}
	if _, exists := s.cache[accountID]; !exists && len(s.cache) >= maxCacheEntries {
		var oldest int64
		var when time.Time
		first := true
		for id, item := range s.cache {
			if first || item.FetchedAt.Before(when) {
				oldest, when, first = id, item.FetchedAt, false
			}
		}
		delete(s.cache, oldest)
	}
	s.cache[accountID] = snapshot
}

func clone(snapshot Snapshot) Snapshot {
	snapshot.Users = append([]User(nil), snapshot.Users...)
	if snapshot.Users == nil {
		snapshot.Users = []User{}
	}
	return snapshot
}

// Classify maps a failed read to a bounded state without exposing upstream
// bodies, URLs or credentials.
func Classify(err error) (string, time.Duration) {
	var upstream *amocrm.APIError
	if errors.As(err, &upstream) {
		switch upstream.Kind {
		case amocrm.ErrorUnauthorized, amocrm.ErrorForbidden:
			return StateAuthError, 0
		case amocrm.ErrorRateLimited, amocrm.ErrorOverloaded:
			return StateRateLimited, upstream.RetryAfter
		case amocrm.ErrorTemporary:
			return StateNetworkError, 0
		default:
			return StateInternalError, 0
		}
	}
	switch {
	case errors.Is(err, amocrm.ErrOverloaded):
		return StateRateLimited, 0
	case errors.Is(err, oauth.ErrRefreshOutcomeUnknown), errors.Is(err, pgx.ErrNoRows):
		return StateAuthError, 0
	case errors.Is(err, amocrm.ErrIncompleteResponse), errors.Is(err, amocrm.ErrResponseTooLarge):
		return StateIncomplete, 0
	case errors.Is(err, amocrm.ErrTransport), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return StateNetworkError, 0
	}
	var network net.Error
	if errors.As(err, &network) {
		return StateNetworkError, 0
	}
	return StateInternalError, 0
}
