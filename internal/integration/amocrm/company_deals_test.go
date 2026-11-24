package amocrm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestCompanyLeadMarkerField(t *testing.T) {
	const title = "Контроль активности 2.0 · fixture · 19.11.2026"
	const marker = "РКРС-installation-id"
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/leads" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPost:
			var leads []struct {
				companyLead
				Responsible int64 `json:"responsible_user_id"`
				Pipeline    int64 `json:"pipeline_id"`
				Stage       int64 `json:"status_id"`
			}
			err := json.NewDecoder(r.Body).Decode(&leads)
			if err != nil || len(leads) != 1 || leads[0].Name != title || leads[0].Pipeline != 100 ||
				leads[0].Responsible != 42 || leads[0].Stage != 101 || len(leads[0].Fields) != 3 || !leads[0].hasFieldValue(5, marker) ||
				!leads[0].hasFieldValue(6, "2542") || !leads[0].hasFieldValue(7, "0") {
				t.Errorf("unexpected create request %+v, error %v", leads, err)
			}
			fmt.Fprint(w, `{"_embedded":{"leads":[{"id":10}]}}`)
		case http.MethodGet:
			if got := r.URL.Query().Get("query"); got != marker {
				t.Errorf("unexpected search query %s", r.URL.RawQuery)
			}
			// Exact field match must survive a manual rename and ignore unrelated fields.
			fmt.Fprint(w, `{"_embedded":{"leads":[
                {"id":11,"name":"Контроль активности 2.0 · fixture · 19.11.2026","custom_fields_values":[{"field_id":6,"values":[{"value":"РКРС-installation-id"}]}]},
                {"id":10,"name":"Renamed","custom_fields_values":[{"field_id":5,"values":[{"value":"РКРС-installation-id"}]},{"field_id":6,"values":[{"value":false}]}]}
            ]}}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	installation := uuid.New()
	id, err := c.CreateCompanyLead(t.Context(), installation, title, 100, 101, 42, []CompanyLeadField{
		{ID: 5, Value: marker},
		{ID: 6, Value: "2542"},
		{ID: 7, Value: "0"},
	})
	if err != nil || id != 10 {
		t.Fatalf("create id=%d err=%v", id, err)
	}
	id, err = c.FindCompanyLead(t.Context(), installation, marker, title, 5)
	if err != nil || id != 10 {
		t.Fatalf("lookup id=%d err=%v", id, err)
	}
}

func TestCompanyLeadMarkerLookupRejectsAmbiguousResult(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"_embedded":{"leads":[
            {"id":10,"custom_fields_values":[{"field_id":5,"values":[{"value":"marker"}]}]},
            {"id":11,"custom_fields_values":[{"field_id":5,"values":[{"value":"marker"}]}]}
        ]}}`)
	})
	id, err := c.FindCompanyLead(t.Context(), uuid.New(), "marker", "Title", 5)
	if id != 0 || !errors.Is(err, ErrIncompleteResponse) {
		t.Fatalf("lookup id=%d err=%v", id, err)
	}
}

func TestCompanyLeadLegacyLookup(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "marker" {
			t.Errorf("unexpected legacy query %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"_embedded":{"leads":[{"id":10,"name":"Title · marker"},{"id":11,"name":"Title · marker extra"}]}}`)
	})
	id, err := c.FindCompanyLead(t.Context(), uuid.New(), "marker", "Title · marker", 0)
	if id != 10 || err != nil {
		t.Fatalf("lookup id=%d err=%v", id, err)
	}
}
