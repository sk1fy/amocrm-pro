package companycrm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const DealPath = "/v1/company-crm/installation-deals"

type DealRequest struct {
	Episode  uuid.UUID `json:"episode_id"`
	Account  int64     `json:"source_account_id"`
	Binding  string    `json:"binding_revision"`
	Title    string    `json:"title"`
	Pipeline int64     `json:"pipeline_id,omitempty"`
	Stage    int64     `json:"stage_id,omitempty"`
	Create   bool      `json:"create"`
}

type DealResult struct {
	State string `json:"state"`
	ID    int64  `json:"id,omitempty"`
	Host  string `json:"host,omitempty"`
}

type DealService struct {
	CRM      *amocrm.Client
	Host     string
	Resolver Resolver
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
		len(input.Title) > 512 || !strings.HasSuffix(input.Title, " · "+DealMarker(input.Episode)) ||
		(input.Create && (input.Pipeline <= 0 || input.Stage <= 0)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	binding, err := s.Deals.Resolver.Resolve(r.Context())
	if err != nil || binding.AccountID != input.Account || binding.Revision != input.Binding {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, err := s.Deals.execute(r.Context(), binding, input)
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

func (s *DealService) execute(ctx context.Context, binding Binding, in DealRequest) (int64, error) {
	if in.Create {
		return s.CRM.CreateCompanyLead(ctx, binding.InstallationID, in.Title, in.Pipeline, in.Stage)
	}
	return s.CRM.FindCompanyLead(ctx, binding.InstallationID, DealMarker(in.Episode), in.Title)
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
