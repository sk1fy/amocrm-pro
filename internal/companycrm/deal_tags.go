package companycrm

import (
	"io"
	"net/http"

	"github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const DealTagsPath = DealPath + "/tags"

type DealTagsRequest struct {
	Account      int64  `json:"source_account_id"`
	Binding      string `json:"binding_revision"`
	DealID       int64  `json:"deal_id"`
	Reauthorized bool   `json:"reauthorized"`
}

func (s *Service) installationDealTags(w http.ResponseWriter, r *http.Request) {
	if s.Deals == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var input DealTagsRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || platformlifecycle.Decode(raw, &input) != nil || input.Account <= 0 || input.Binding == "" || input.DealID <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	binding, err := s.Deals.Resolver.Resolve(r.Context())
	if err != nil || binding.AccountID != input.Account || binding.Revision != input.Binding {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	tags := []map[string]string{{"name": "Установка"}}
	if input.Reauthorized {
		tags = append(tags, map[string]string{"name": "Повторная авторизация"})
	}
	var response struct {
		Embedded struct {
			Leads []struct {
				ID int64 `json:"id"`
			} `json:"leads"`
		} `json:"_embedded"`
	}
	patch := []map[string]any{{"id": input.DealID, "tags_to_add": tags}}
	if err = s.Deals.CRM.DoJSON(r.Context(), binding.InstallationID, http.MethodPatch, "/api/v4/leads", patch, &response); err != nil || len(response.Embedded.Leads) != 1 || response.Embedded.Leads[0].ID != input.DealID {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
