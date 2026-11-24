package companycrm

import (
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Binding struct {
	AccountID      int64     `json:"account_id"`
	InstallationID uuid.UUID `json:"installation_id"`
	IntegrationID  uuid.UUID `json:"integration_id"`
	Revision       string    `json:"revision"`
	AccountFieldID int64     `json:"account_field_id"`
}

type Source struct {
	AccountID       int64      `json:"account_id"`
	BindingRevision string     `json:"binding_revision"`
	Complete        bool       `json:"complete"`
	State           string     `json:"state"`
	ObservedAt      *time.Time `json:"observed_at"`
	FreshUntil      *time.Time `json:"fresh_until"`
	RetryAfter      int64      `json:"retry_after"`
	ErrorCode       string     `json:"error_code,omitempty"`
}

type Response[T any] struct {
	Source Source `json:"source"`
	Data   *T     `json:"data"`
}

type StaffMember struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Activity string `json:"activity"`
}

type Contact struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	CompanyIDs []int64 `json:"company_ids"`
}

type Client struct {
	CompanyID  int64   `json:"company_id"`
	Name       string  `json:"name"`
	AccountIDs []int64 `json:"account_ids"`
	Resolution string  `json:"resolution"`
}

type Clients struct {
	Companies []Client  `json:"companies"`
	Contacts  []Contact `json:"contacts"`
}

type WorkflowEmployee struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type WorkflowMetadata struct {
	Employees           *[]WorkflowEmployee         `json:"employees,omitempty"`
	RequiredFieldsState string                      `json:"required_fields_state"`
	Pipelines           []amocrm.CompanyCRMPipeline `json:"pipelines"`
	LeadFields          []amocrm.CompanyCRMField    `json:"lead_fields"`
}

type WorkflowRequest struct {
	BindingRevision string `json:"binding_revision"`
	PipelineID      int64  `json:"pipeline_id"`
	StageID         int64  `json:"stage_id"`
}

type WorkflowValidation struct {
	Scope      string `json:"scope"`
	WriteReady bool   `json:"write_ready"`
	Source     Source `json:"source"`
	Valid      bool   `json:"valid"`
	Code       string `json:"code"`
}
