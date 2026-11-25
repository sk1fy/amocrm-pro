package companycrm

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const DealTitlePath = DealPath + "/title"

type DealTitleRequest struct {
	Account int64  `json:"source_account_id"`
	Binding string `json:"binding_revision"`
	DealID  int64  `json:"deal_id"`
	Event   string `json:"event"`
}

func (s *Service) installationDealTitle(w http.ResponseWriter, r *http.Request) {
	if s.Deals == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var input DealTitleRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || platformlifecycle.Decode(raw, &input) != nil || input.Account <= 0 || input.Binding == "" || input.DealID <= 0 || (input.Event != "reinstalled" && input.Event != "reauthorized") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	binding, err := s.Deals.Resolver.Resolve(r.Context())
	if err != nil || binding.AccountID != input.Account || binding.Revision != input.Binding {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	prefix := "Повторная авторизация · "
	if input.Event == "reinstalled" {
		prefix = "Переустановка · "
	}
	var lead struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err = s.Deals.CRM.DoJSON(r.Context(), binding.InstallationID, http.MethodGet, "/api/v4/leads/"+strconv.FormatInt(input.DealID, 10), nil, &lead); err != nil || lead.ID != input.DealID || strings.TrimSpace(lead.Name) == "" {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if strings.HasPrefix(lead.Name, prefix) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var response struct {
		Embedded struct {
			Leads []struct {
				ID int64 `json:"id"`
			} `json:"leads"`
		} `json:"_embedded"`
	}
	patch := []map[string]any{{"id": input.DealID, "name": prefix + lead.Name}}
	if err = s.Deals.CRM.DoJSON(r.Context(), binding.InstallationID, http.MethodPatch, "/api/v4/leads", patch, &response); err != nil || len(response.Embedded.Leads) != 1 || response.Embedded.Leads[0].ID != input.DealID {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
