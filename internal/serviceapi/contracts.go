// Package serviceapi contains transport-neutral v0 contracts. No component DB,
// generated transport or OAuth credential types cross these ports.
package serviceapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"time"
)

const (
	ActivityService = "activity"
	EventsService   = "crm-events"
	GatewayService  = "gateway"
	CoreService     = "core"
)
const (
	ActionPanel     = "panel"
	ActionSettings  = "settings"
	ActionRead      = "read"
	ActionStatus    = "status"
	ActionSync      = "sync"
	ActionOperation = "operation"
	ActionEvents    = "events"
	ActionUsers     = "users"
)

type Code string

const (
	InvalidArgument   Code = "invalid_argument"
	Unauthenticated   Code = "unauthenticated"
	PermissionDenied  Code = "permission_denied"
	NotFound          Code = "not_found"
	Conflict          Code = "conflict"
	Unavailable       Code = "unavailable"
	DeadlineExceeded  Code = "deadline_exceeded"
	ResourceExhausted Code = "resource_exhausted"
	ReauthRequired    Code = "reauth_required"
	Internal          Code = "internal"
)

type Error struct {
	Code       Code          `json:"code"`
	Message    string        `json:"message"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

func (e *Error) Error() string             { return string(e.Code) + ": " + e.Message }
func Fail(code Code, message string) error { return &Error{Code: code, Message: message} }
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return Unavailable
	}
	return Internal
}

type Scope struct {
	IntegrationID  uuid.UUID `json:"integration_id"`
	InstallationID uuid.UUID `json:"installation_id"`
}
type Auth struct {
	Token string `json:"token"`
}
type Principal struct {
	Scope
	ActorID   int64     `json:"actor_id"`
	System    bool      `json:"system"`
	Consumer  string    `json:"consumer"`
	RequestID string    `json:"request_id"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Grant struct {
	Audience string `json:"audience"`
	Action   string `json:"action"`
}

// Issue is only available to authenticated Core and CRM Events identities. Core
// supplies verified widget actors; CRM Events can request only system grants
// for already admitted sources, and live policy is checked before every page.
type IssueRequest struct {
	Scope
	ActorID   int64   `json:"actor_id"`
	System    bool    `json:"system"`
	Consumer  string  `json:"consumer"`
	RequestID string  `json:"request_id"`
	Grants    []Grant `json:"grants"`
}
type Policy interface {
	Issue(context.Context, IssueRequest) (Auth, error)
	Validate(context.Context, Auth, string, string) (Principal, error)
}

type Event struct {
	ID          string          `json:"id"`
	CreatedAt   int64           `json:"created_at"`
	CreatedBy   int64           `json:"created_by"`
	Type        string          `json:"type"`
	EntityID    int64           `json:"entity_id"`
	EntityType  string          `json:"entity_type"`
	ValueBefore json.RawMessage `json:"value_before"`
	ValueAfter  json.RawMessage `json:"value_after"`
}
type EventPageRequest struct {
	Auth  Auth  `json:"auth"`
	From  int64 `json:"from"`
	To    int64 `json:"to"`
	Page  int   `json:"page"`
	Limit int   `json:"limit"`
}
type EventPage struct {
	Events  []Event `json:"events"`
	HasNext bool    `json:"has_next"`
}
type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name"`
}
type Directory struct {
	Users    []User `json:"users"`
	Timezone string `json:"timezone"`
}
type UsersRequest struct {
	Auth    Auth    `json:"auth"`
	UserIDs []int64 `json:"user_ids"`
}
type Gateway interface {
	Events(context.Context, EventPageRequest) (EventPage, error)
	Users(context.Context, UsersRequest) (Directory, error)
}

type Query struct {
	Auth    Auth    `json:"auth"`
	From    int64   `json:"from"`
	To      int64   `json:"to"`
	UserIDs []int64 `json:"user_ids"`
	Limit   int     `json:"limit"`
	Cursor  string  `json:"cursor"`
}
type UserSummary struct {
	UserID       int64 `json:"user_id"`
	UniqueEvents int64 `json:"unique_events"`
	LastEventAt  int64 `json:"last_event_at"`
}
type QueryResult struct {
	Events     []Event       `json:"events"`
	Summaries  []UserSummary `json:"summaries"`
	NextCursor string        `json:"next_cursor,omitempty"`
	Status     SyncStatus    `json:"status"`
}
type SyncStatus struct {
	Verification    string `json:"verification"`
	Enabled         bool   `json:"enabled"`
	State           string `json:"state"`
	HistoryFrom     int64  `json:"history_from"`
	VerifiedFrom    int64  `json:"verified_from"`
	VerifiedThrough int64  `json:"verified_through"`
	WindowFrom      int64  `json:"window_from"`
	WindowTo        int64  `json:"window_to"`
	NextPage        int    `json:"next_page"`
	LastSuccessAt   int64  `json:"last_success_at"`
	LastEventAt     int64  `json:"last_event_at"`
	LagSeconds      int64  `json:"lag_seconds"`
	ErrorCode       string `json:"error_code,omitempty"`
	ReauthRequired  bool   `json:"reauth_required"`
}
type Command struct {
	Auth          Auth   `json:"auth"`
	CommandID     string `json:"command_id"`
	Kind          string `json:"kind"`
	From          int64  `json:"from"`
	To            int64  `json:"to"`
	InitialDays   int    `json:"initial_days"`
	RetentionDays int    `json:"retention_days"`
}
type Operation struct {
	ID           string `json:"id"`
	CommandID    string `json:"command_id"`
	State        string `json:"state"`
	ErrorCode    string `json:"error_code,omitempty"`
	Processed    int64  `json:"processed"`
	Inserted     int64  `json:"inserted"`
	Updated      int64  `json:"updated"`
	Deduplicated int64  `json:"deduplicated"`
}
type OperationRequest struct {
	Auth        Auth   `json:"auth"`
	OperationID string `json:"operation_id"`
}
type CRMEvents interface {
	Apply(context.Context, Command) (Operation, error)
	Query(context.Context, Query) (QueryResult, error)
	Status(context.Context, Auth) (SyncStatus, error)
	Operation(context.Context, OperationRequest) (Operation, error)
}

type Settings struct {
	InitialDays   int `json:"initial_days"`
	RetentionDays int `json:"retention_days"`
}
type SettingsCommand struct {
	Auth      Auth     `json:"auth"`
	CommandID string   `json:"command_id"`
	Settings  Settings `json:"settings"`
}
type Panel struct {
	Coverage string      `json:"coverage"`
	Users    []User      `json:"users"`
	Timezone string      `json:"timezone"`
	Data     QueryResult `json:"data"`
	Settings Settings    `json:"settings"`
}
type Activity interface {
	Panel(context.Context, Query) (Panel, error)
	Settings(context.Context, Auth) (Settings, error)
	Configure(context.Context, SettingsCommand) (Operation, error)
	Operation(context.Context, OperationRequest) (Operation, error)
}

// ValidateQuery bounds every domain/transport implementation equally.
func ValidateQuery(q Query) error {
	if q.From < 0 || q.To <= q.From || q.To-q.From > 31*86400 || len(q.UserIDs) > 100 || len(q.Cursor) > 512 || q.Limit < 0 || q.Limit > 100 {
		return Fail(InvalidArgument, "query exceeds v0 period, users, cursor or page bounds")
	}
	for _, id := range q.UserIDs {
		if id <= 0 {
			return Fail(InvalidArgument, "user id must be positive")
		}
	}
	return nil
}

// UserGrantsFor returns only the grants needed by a single service operation,
// including its synchronous downstream calls. Unsupported operations fail
// closed (Issue rejects an empty grant list).
func UserGrantsFor(audience, action string) []Grant {
	switch {
	case audience == ActivityService && action == ActionPanel:
		return []Grant{{ActivityService, ActionPanel}, {EventsService, ActionRead}, {GatewayService, ActionUsers}}
	case audience == ActivityService && (action == ActionSettings || action == ActionOperation):
		return []Grant{{audience, action}}
	case audience == EventsService && (action == ActionRead || action == ActionStatus || action == ActionSync || action == ActionOperation):
		return []Grant{{audience, action}}
	}
	return nil
}

// UserGrants is retained for contract compatibility with earlier Core callers.
// New ingress and delivery code must use UserGrantsFor for least privilege.
func UserGrants() []Grant {
	return []Grant{{ActivityService, ActionPanel}, {ActivityService, ActionSettings}, {ActivityService, ActionOperation}, {EventsService, ActionRead}, {EventsService, ActionStatus}, {EventsService, ActionSync}, {EventsService, ActionOperation}, {GatewayService, ActionUsers}}
}
