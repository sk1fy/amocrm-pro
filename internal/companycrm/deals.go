package companycrm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const DealPath = "/v1/company-crm/installation-deals"

type DealRequest struct {
	ResponsibleUserID int64     `json:"responsible_user_id,omitempty"`
	Episode           uuid.UUID `json:"episode_id"`
	Account           int64     `json:"source_account_id"`
	Binding           string    `json:"binding_revision"`
	Title             string    `json:"title"`
	Pipeline          int64     `json:"pipeline_id,omitempty"`
	Stage             int64     `json:"stage_id,omitempty"`
	Create            bool      `json:"create"`
	ClientAccountID   int64     `json:"client_amo_account_id,omitempty"`
	ActiveUsers       *int      `json:"active_users,omitempty"`
}

type DealResult struct {
	State string `json:"state"`
	ID    int64  `json:"id,omitempty"`
	Host  string `json:"host,omitempty"`
}

type DealService struct {
	CRM              *amocrm.Client
	Host             string
	Resolver         Resolver
	MarkerFieldID    int64
	AccountIDFieldID int64
	UserCountFieldID int64
}

func DealMarker(episode uuid.UUID) string { return "РКРС-" + episode.String() }

func (s *Service) installationDeal(w http.ResponseWriter, r *http.Request) {
	if s.Deals == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var input DealRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || wire.Decode(raw, &input) != nil || input.Episode == uuid.Nil || input.Account <= 0 ||
		len(input.Title) > 512 || strings.TrimSpace(input.Title) == "" ||
		input.ResponsibleUserID < 0 || input.ClientAccountID < 0 || (input.ActiveUsers != nil &&
		(input.ClientAccountID <= 0 || *input.ActiveUsers < 0 || *input.ActiveUsers > 1000)) ||
		(input.Create && (input.Pipeline <= 0 || input.Stage <= 0)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	marker := DealMarker(input.Episode)
	markerFieldID := s.Deals.MarkerFieldID
	// Reservations written before the field was introduced still reconcile by title.
	if strings.HasSuffix(input.Title, " · "+marker) {
		markerFieldID = 0
	} else if markerFieldID <= 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	binding, err := s.Deals.Resolver.Resolve(r.Context())
	if err != nil || binding.AccountID != input.Account || binding.Revision != input.Binding {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, err := s.Deals.execute(r.Context(), binding, input, marker, markerFieldID)
	result := DealResult{State: "found", ID: id, Host: s.Deals.Host}
	var api *amocrm.APIError
	switch {
	case input.Create && (errors.Is(err, amocrm.ErrOverloaded) || (errors.As(err, &api) && api.StatusCode == http.StatusTooManyRequests)):
		result = DealResult{State: "rate_limited"}
	case input.Create && explicitLeadRejection(err):
		result = DealResult{State: "rejected"}
	case err != nil:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case id == 0:
		result = DealResult{State: "absent"}
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *DealService) execute(ctx context.Context, binding Binding, in DealRequest, marker string, markerFieldID int64) (int64, error) {
	if !in.Create {
		return s.CRM.FindCompanyLead(ctx, binding.InstallationID, marker, in.Title, markerFieldID)
	}
	var fields []amocrm.CompanyLeadField
	if markerFieldID > 0 {
		fields = append(fields, amocrm.CompanyLeadField{ID: markerFieldID, Value: marker})
	}
	if s.AccountIDFieldID > 0 && in.ClientAccountID > 0 {
		fields = append(fields, amocrm.CompanyLeadField{ID: s.AccountIDFieldID, Value: strconv.FormatInt(in.ClientAccountID, 10)})
	}
	if s.UserCountFieldID > 0 && in.ActiveUsers != nil {
		fields = append(fields, amocrm.CompanyLeadField{ID: s.UserCountFieldID, Value: strconv.Itoa(*in.ActiveUsers)})
	}
	return s.CRM.CreateCompanyLead(ctx, binding.InstallationID, in.Title, in.Pipeline, in.Stage, in.ResponsibleUserID, fields)
}

func explicitLeadRejection(err error) bool {
	var api *amocrm.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.StatusCode {
	case 400, 401, 402, 403, 404, 422, 429:
		return true
	default:
		return false
	}
}
