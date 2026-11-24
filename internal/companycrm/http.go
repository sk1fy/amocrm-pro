package companycrm

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

func identity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	uris := r.TLS.PeerCertificates[0].URIs
	if len(uris) != 1 {
		return ""
	}
	switch uris[0].String() {
	case "spiffe://amocrm-pro/admin-company-directory":
		return "admin-company-directory"
	case "spiffe://amocrm-pro/platform-company-crm":
		return "platform-company-crm"
	}
	return ""
}

func Router(s *Service) http.Handler {
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		actor := identity(r)
		staff := r.URL.Path == "/v1/company-crm/staff" && r.Method == http.MethodGet
		platform := (r.URL.Path == "/v1/company-crm/clients" || r.URL.Path == "/v1/company-crm/workflow-metadata") && r.Method == http.MethodGet || r.URL.Path == "/v1/company-crm/validate-workflow" && r.Method == http.MethodPost
		platform = platform || ((r.URL.Path == DealPath || r.URL.Path == DealTagsPath) && r.Method == http.MethodPost)
		if !(staff && actor == "admin-company-directory" || platform && actor == "platform-company-crm") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "permission_denied"})
			return
		}
		if r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		write := func(source Source, body any) {
			if source.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(source.RetryAfter, 10))
			}
			if !source.Complete {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_ = json.NewEncoder(w).Encode(body)
		}
		switch r.URL.Path {
		case DealTagsPath:
			s.installationDealTags(w, r)
		case DealPath:
			s.installationDeal(w, r)
		case "/v1/company-crm/staff":
			v := s.Staff(r.Context())
			write(v.Source, v)
		case "/v1/company-crm/clients":
			v := s.Clients(r.Context())
			write(v.Source, v)
		case "/v1/company-crm/workflow-metadata":
			v := s.WorkflowMetadata(r.Context())
			write(v.Source, v)
		case "/v1/company-crm/validate-workflow":
			var input WorkflowRequest
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || input.PipelineID <= 0 || input.StageID <= 0 || input.BindingRevision == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			v := s.ValidateWorkflow(r.Context(), input)
			write(v.Source, v)
		}
	})
}
