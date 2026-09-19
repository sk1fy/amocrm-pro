package adminread

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/connectioncheck"
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
	AuthorizationCheck connectioncheck.Snapshot `json:"authorization_check"`
	ID                 uuid.UUID                `json:"id"`
	IntegrationID      uuid.UUID                `json:"integration_id"`
	IntegrationCode    string                   `json:"integration_code"`
	Status             string                   `json:"status"`
	WebhookStatus      string                   `json:"webhook_status,omitempty"`
	Authorization      string                   `json:"authorization_state,omitempty"`
	RecentFailedJobs   int                      `json:"recent_failed_jobs"`
	Grants             []Grant                  `json:"grants"`
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
	AuthorizationCheck connectioncheck.Snapshot `json:"authorization_check"`
	ID                 uuid.UUID                `json:"id"`
	IntegrationID      uuid.UUID                `json:"integration_id"`
	IntegrationCode    string                   `json:"integration_code"`
	AccountID          int64                    `json:"account_id"`
	AccountDomain      string                   `json:"account_domain"`
	Status             string                   `json:"status"`
	InstalledBy        *int64                   `json:"installed_by"`
	Origin             string                   `json:"origin"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	WebhookStatus      string                   `json:"webhook_status,omitempty"`
	RecentFailedJobs   int                      `json:"recent_failed_jobs"`
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
	"activity_settings",
	"activity_sync",
	"activity_panels",
	"lead_status",
	"stats",
}

type ActivitySettingsResponse struct {
	Source        string    `json:"source"`
	ObservedAt    time.Time `json:"observed_at"`
	InitialDays   int       `json:"initial_days"`
	RetentionDays int       `json:"retention_days"`
	UpdatedAt     int64     `json:"updated_at"`
}

type ActivityStatusResponse struct {
	Source          string    `json:"source"`
	ObservedAt      time.Time `json:"observed_at"`
	Enabled         bool      `json:"enabled"`
	State           string    `json:"state"`
	Verification    string    `json:"verification"`
	LagSeconds      int64     `json:"lag_seconds"`
	ReauthRequired  bool      `json:"reauth_required"`
	ErrorCode       string    `json:"error_code,omitempty"`
	LastSuccessAt   *string   `json:"last_success_at"`
	LastEventAt     *string   `json:"last_event_at"`
	VerifiedFrom    *string   `json:"verified_from"`
	VerifiedThrough *string   `json:"verified_through"`
}

type ActivityOperationResponse struct {
	Source        string    `json:"source"`
	ObservedAt    time.Time `json:"observed_at"`
	CommandID     string    `json:"command_id"`
	OperationID   string    `json:"operation_id"`
	State         string    `json:"state"`
	DeliveryState string    `json:"delivery_state"`
	ErrorCode     string    `json:"error_code,omitempty"`
}

type AdminPanel struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	EmployeeIDs    []int64   `json:"employee_ids"`
	DisplayWindow  any       `json:"display_window"`
	Timezone       string    `json:"timezone,omitempty"`
	Enabled        bool      `json:"enabled"`
	Revision       int64     `json:"revision"`
	UpdatedAt      time.Time `json:"updated_at"`
	ShareURLIssued bool      `json:"share_url_issued"`
}

type LeadStatusRule struct {
	ID               string    `json:"id"`
	SourcePipelineID int64     `json:"source_pipeline_id"`
	SourceStatusID   int64     `json:"source_status_id"`
	TargetPipelineID int64     `json:"target_pipeline_id"`
	TargetStatusID   int64     `json:"target_status_id"`
	Enabled          bool      `json:"enabled"`
	Revision         int64     `json:"revision"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type LeadStatusRun struct {
	ID           string     `json:"id"`
	WorkflowType string     `json:"workflow_type"`
	Status       string     `json:"status"`
	RuleID       *string    `json:"rule_id"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	EffectState  *string    `json:"effect_state"`
	EffectError  *string    `json:"effect_error"`
	EffectType   *string    `json:"effect_type,omitempty"`
}

type VerificationCounts struct {
	Unverified      int64 `json:"unverified"`
	TemporaryErrors int64 `json:"temporary_errors"`
	Verified        int64 `json:"verified"`
}
type StatsResponse struct {
	Verification      *VerificationCounts `json:"verification,omitempty"`
	Source            string              `json:"source"`
	ObservedAt        time.Time           `json:"observed_at"`
	Period            string              `json:"period"`
	From              time.Time           `json:"from"`
	To                time.Time           `json:"to"`
	PeriodStart       time.Time           `json:"period_start"`
	PeriodEnd         time.Time           `json:"period_end"`
	Connections       []StatsConnection   `json:"connections"`
	PeriodEvents      StatsPeriodEvents   `json:"period_events"`
	Connected         *int64              `json:"connected"`
	Disconnected      *int64              `json:"disconnected"`
	ActiveAccounts    *int64              `json:"active_accounts"`
	LastUseAt         *time.Time          `json:"last_use_at"`
	JobErrors         *int64              `json:"job_errors"`
	Latency           StatsLatency        `json:"latency"`
	LatencyP50Ms      *int64              `json:"latency_p50_ms"`
	Queues            []StatsQueue        `json:"queues"`
	AuthProblemsCount *int64              `json:"auth_problems_count"`
	SyncProblemsCount *int64              `json:"sync_problems_count"`
	AuthProblems      *int64              `json:"auth_problems"`
	SyncProblems      *int64              `json:"sync_problems"`
}

type StatsConnection struct {
	IntegrationCode string `json:"integration_code"`
	Product         string `json:"product"`
	Status          string `json:"status"`
	Count           int64  `json:"count"`
}

type StatsPeriodEvents struct {
	Connected    *int64 `json:"connected"`
	Disconnected *int64 `json:"disconnected"`
}

type StatsLatency struct {
	AvgMS *float64 `json:"avg_ms"`
	P50MS *float64 `json:"p50_ms"`
}

type StatsQueue struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type StatsAccount struct {
	AccountID       int64  `json:"account_id"`
	Domain          string `json:"domain"`
	InstallationID  string `json:"installation_id"`
	IntegrationCode string `json:"integration_code"`
	Reason          string `json:"reason"`
}
