package adminread

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
)

const (
	sourceCore      = "core"
	contractVersion = "v1"
	originFixture   = "fixture"
	originReal      = "real"

	pilotEnabled       = "enabled"
	pilotDisabled      = "disabled"
	pilotNotConfigured = "not_configured"

	authMissing            = "missing"
	authReauthRequired     = "reauth_required"
	authRefreshing         = "refreshing"
	authExpiredRefreshable = "expired_refreshable"
	authValid              = "valid"
)

type listResponse struct {
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Items      any       `json:"items"`
	NextCursor *string   `json:"next_cursor"`
	Total      *int64    `json:"total"`
}

type BackendResponse struct {
	Source          string    `json:"source"`
	ObservedAt      time.Time `json:"observed_at"`
	Backend         string    `json:"backend"`
	Revision        string    `json:"revision"`
	ContractVersion string    `json:"contract_version"`
	Capabilities    []string  `json:"capabilities"`
	Components      any       `json:"components"`
}

type AccountListItem struct {
	AccountID           int64                 `json:"account_id"`
	Domains             []string              `json:"domains"`
	ConnectionsByStatus map[string]int        `json:"connections_by_status"`
	LastActivityAt      time.Time             `json:"last_activity_at"`
	Origin              string                `json:"origin"`
	Installations       []AccountInstallation `json:"installations"`
}

type AccountInstallation struct {
	ID               uuid.UUID `json:"id"`
	IntegrationID    uuid.UUID `json:"integration_id"`
	IntegrationCode  string    `json:"integration_code"`
	Status           string    `json:"status"`
	WebhookStatus    string    `json:"webhook_status,omitempty"`
	Authorization    string    `json:"authorization_state,omitempty"`
	RecentFailedJobs int       `json:"recent_failed_jobs"`
	Grants           []Grant   `json:"grants"`
}

type AccountResponse struct {
	Source        string             `json:"source"`
	ObservedAt    time.Time          `json:"observed_at"`
	AccountID     int64              `json:"account_id"`
	Domains       []string           `json:"domains"`
	Origin        string             `json:"origin"`
	Installations []InstallationCard `json:"installations"`
}

type InstallationSummary struct {
	ID               uuid.UUID `json:"id"`
	IntegrationID    uuid.UUID `json:"integration_id"`
	IntegrationCode  string    `json:"integration_code"`
	AccountID        int64     `json:"account_id"`
	AccountDomain    string    `json:"account_domain"`
	Status           string    `json:"status"`
	InstalledBy      *int64    `json:"installed_by"`
	Origin           string    `json:"origin"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	WebhookStatus    string    `json:"webhook_status,omitempty"`
	RecentFailedJobs int       `json:"recent_failed_jobs"`
}

type InstallationCard struct {
	Installation  InstallationSummary `json:"installation"`
	Authorization Authorization       `json:"authorization"`
	Webhook       WebhookInfo         `json:"webhook"`
	Grants        []Grant             `json:"grants"`
	Activity      ActivityInfo        `json:"activity"`
}

type InstallationResponse struct {
	Source        string              `json:"source"`
	ObservedAt    time.Time           `json:"observed_at"`
	Installation  InstallationSummary `json:"installation"`
	Authorization Authorization       `json:"authorization"`
	Webhook       WebhookInfo         `json:"webhook"`
	Grants        []Grant             `json:"grants"`
	Activity      ActivityInfo        `json:"activity"`
}

// Authorization is the computed OAuth credential state. CredentialVersion is
// oauth_credentials.token_version; the JSON name avoids the substring "token"
// required by the admin secret-key scan.
type Authorization struct {
	State              string     `json:"state"`
	CredentialsPresent bool       `json:"credentials_present"`
	ExpiresAt          *time.Time `json:"expires_at"`
	CredentialVersion  int64      `json:"credential_version,omitempty"`
	RefreshedAt        *time.Time `json:"refreshed_at"`
	KeyVersion         int        `json:"key_version,omitempty"`
	LeaseActive        bool       `json:"lease_active"`
	Unverified         bool       `json:"unverified"`
}

type AuthorizationFacts struct {
	InstallationStatus string
	Present            bool
	ExpiresAt          *time.Time
	TokenVersion       int64
	KeyVersion         int
	RefreshedAt        *time.Time
	LeaseUntil         *time.Time
}

type WebhookInfo struct {
	Status                string     `json:"status"`
	Events                []string   `json:"events"`
	CheckedAt             *time.Time `json:"checked_at"`
	LastError             *string    `json:"last_error"`
	ConfirmedDestinations int        `json:"confirmed_destinations"`
}

type Grant struct {
	Service string `json:"service"`
	Enabled bool   `json:"enabled"`
}

type ActivityInfo struct {
	Pilot string `json:"pilot"`
}

type Job struct {
	RetryAllowed     bool       `json:"retry_allowed"`
	RetryReason      string     `json:"retry_reason,omitempty"`
	ID               uuid.UUID  `json:"id"`
	InstallationID   *uuid.UUID `json:"installation_id"`
	AccountID        *int64     `json:"account_id,omitempty"`
	Type             string     `json:"type"`
	ActorType        *string    `json:"actor_type"`
	ActorID          *string    `json:"actor_id"`
	ResourceType     *string    `json:"resource_type"`
	ResourceID       *string    `json:"resource_id"`
	Status           string     `json:"status"`
	Priority         int16      `json:"priority"`
	Attempts         int        `json:"attempts"`
	MaxAttempts      int        `json:"max_attempts"`
	RunAfter         time.Time  `json:"run_after"`
	LastErrorCode    *string    `json:"last_error_code"`
	LastErrorMessage *string    `json:"last_error_message"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

type JobAttempt struct {
	ID           int64      `json:"id"`
	JobID        uuid.UUID  `json:"job_id"`
	Attempt      int        `json:"attempt"`
	WorkerID     string     `json:"worker_id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Outcome      *string    `json:"outcome"`
	ErrorCode    *string    `json:"error_code"`
	ErrorMessage *string    `json:"error_message"`
	DurationMS   *int64     `json:"duration_ms"`
}

type JobResponse struct {
	Source     string       `json:"source"`
	ObservedAt time.Time    `json:"observed_at"`
	Job        Job          `json:"job"`
	Attempts   []JobAttempt `json:"attempts"`
}

type JobsSummaryResponse struct {
	Source     string         `json:"source"`
	ObservedAt time.Time      `json:"observed_at"`
	Counts     map[string]int `json:"counts"`
}

type AuditEntry struct {
	ID               int64           `json:"id"`
	InstallationID   *uuid.UUID      `json:"installation_id"`
	ActorType        string          `json:"actor_type"`
	ActorID          *string         `json:"actor_id"`
	Action           string          `json:"action"`
	ObjectType       *string         `json:"object_type"`
	ObjectID         *string         `json:"object_id"`
	Metadata         json.RawMessage `json:"metadata"`
	CorrelationJobID *uuid.UUID      `json:"correlation_job_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

type Integration struct {
	ID                    uuid.UUID      `json:"id"`
	Code                  string         `json:"code"`
	ClientID              string         `json:"client_id"`
	RedirectURI           string         `json:"redirect_uri"`
	Status                string         `json:"status"`
	WebhookEvents         []string       `json:"webhook_events"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
	KeyVersion            int            `json:"key_version"`
	Grants                []Grant        `json:"grants"`
	InstallationsByStatus map[string]int `json:"installations_by_status"`
}

type IntegrationResponse struct {
	Source      string      `json:"source"`
	ObservedAt  time.Time   `json:"observed_at"`
	Integration Integration `json:"integration"`
}

type DeliveriesResponse struct {
	Source     string                    `json:"source"`
	ObservedAt time.Time                 `json:"observed_at"`
	Items      []activitybridge.Delivery `json:"items"`
}

var adminCapabilities = []string{
	"commands",
	"diagnostics",
	"accounts",
	"installations",
	"integrations",
	"jobs",
	"audit",
	"activity_deliveries",
}
