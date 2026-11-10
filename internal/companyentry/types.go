package companyentry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

var ErrDenied = errors.New("company entry denied")
var ErrConflict = errors.New("company association conflict")

type Subject struct {
	AccountID      string    `json:"accountId"`
	ActorID        string    `json:"actorId"`
	IntegrationID  uuid.UUID `json:"integrationId"`
	InstallationID uuid.UUID `json:"installationId"`
}
type Status struct {
	State     string    `json:"state"`
	CompanyID uuid.UUID `json:"companyId,omitempty"`
}
type EntryInput struct {
	Subject
	Intent string `json:"intent"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}
type Entry struct {
	CompanyID uuid.UUID `json:"companyId"`
	UserID    uuid.UUID `json:"userId"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type Authority interface {
	Status(context.Context, string) (Status, error)
	Enter(context.Context, EntryInput) (Entry, error)
}
type IdentityReader interface {
	CompanyEntryProfile(context.Context, uuid.UUID, int64) (amocrm.CompanyEntryProfile, error)
}
type Repository interface {
	Active(context.Context, widgetauth.Principal) error
	Associate(context.Context, int64, uuid.UUID) error
	Associated(context.Context, int64) (uuid.UUID, error)
}

func (s Status) MarshalJSON() ([]byte, error) {
	if s.CompanyID == uuid.Nil {
		return json.Marshal(struct {
			State string `json:"state"`
		}{s.State})
	}
	return json.Marshal(struct {
		State     string    `json:"state"`
		CompanyID uuid.UUID `json:"companyId"`
	}{s.State, s.CompanyID})
}
