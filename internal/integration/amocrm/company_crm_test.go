package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCompanyCRMStaffPreservesUnknownAndReadsEveryPage(t *testing.T) {
	pages := 0
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"_embedded":{"users":[{"id":1,"name":"Unknown"}]},"_links":{"next":{"href":"https://attacker.invalid/untrusted"}}}`)
			return
		}
		if r.URL.Path != "/api/v4/users" || r.URL.Query().Get("page") != "2" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"_embedded":{"users":[{"id":2,"rights":{"is_active":false}}]}}`)
	})
	users, err := c.CompanyCRMStaff(t.Context(), uuid.New())
	if err != nil || len(users) != 2 || users[0].Rights.Active != nil || users[1].Rights.Active == nil || *users[1].Rights.Active || pages != 2 {
		t.Fatalf("users=%+v err=%v pages=%d", users, err, pages)
	}
}
func TestCompanyCRMContactsReadPastFortyPages(t *testing.T) {
	const totalPages = 41
	page := 0
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if r.URL.Path != "/api/v4/contacts" || r.URL.Query().Get("page") != fmt.Sprint(page) {
			t.Errorf("unexpected request: %s", r.URL)
		}
		var next string
		if page < totalPages {
			next = `,"_links":{"next":{"href":"next"}}`
		}
		fmt.Fprintf(w, `{"_page":%d,"_page_count":41,"_total_items":41,"_embedded":{"contacts":[{"id":%d,"account_id":7}]}%s}`, page, page, next)
	})
	contacts, err := c.CompanyCRMContacts(t.Context(), uuid.New(), 7)
	if err != nil || len(contacts) != totalPages || contacts[totalPages-1].ID != totalPages {
		t.Fatalf("scanned %d pages and got %d contacts: %v", page, len(contacts), err)
	}
}

func TestCompanyCRMPageFailureNeverReturnsPartial(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"_embedded":{"users":[{"id":1}]},"_links":{"next":{"href":"next"}}}`)
			return
		}
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
	})
	users, err := c.CompanyCRMStaff(t.Context(), uuid.New())
	var api *APIError
	if users != nil || !errors.As(err, &api) || api.RetryAfter != 42*time.Second {
		t.Fatalf("users=%+v err=%v", users, err)
	}
}
func TestCompanyCRMAdvertisedNextPageCannotEndWithNoContent(t *testing.T) {
	pages := 0
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"_embedded":{"companies":[{"id":1,"account_id":7}]},"_links":{"next":{"href":"next"}}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rows, err := c.CompanyCRMCompanies(t.Context(), uuid.New(), 7)
	if rows != nil || !errors.Is(err, ErrIncompleteResponse) || pages != 2 {
		t.Fatalf("advertised page was lost: rows=%v err=%v pages=%d", rows, err, pages)
	}
}
func TestCompanyCRMRejectsWrongAccountAndMalformedEmpty(t *testing.T) {
	for _, body := range []string{`{"_embedded":{"companies":[{"id":1,"account_id":8}]}}`, `{}`, `{"_embedded":{"companies":null}}`} {
		c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		rows, err := c.CompanyCRMCompanies(t.Context(), uuid.New(), 7)
		if err == nil || rows != nil {
			t.Fatalf("body=%s rows=%+v err=%v", body, rows, err)
		}
	}
}
func TestCompanyCRMDeadlineCancelsProviderRead(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := c.CompanyCRMStaff(ctx, uuid.New())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestCompanyCRMVerifiedMappingUsesExactIntegerIDs(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{`"100"`, true}, {`100`, true}, {`1.5`, false}, {`"name@example.test"`, false}, {`0`, false}} {
		var company CompanyCRMCompany
		body := fmt.Sprintf(`{"id":3,"custom_fields_values":[{"field_id":9,"field_type":"numeric","values":[{"value":%s}]}]}`, tc.value)
		if err := json.Unmarshal([]byte(body), &company); err != nil {
			t.Fatal(err)
		}
		ids, valid := company.CompanyCRMAccountIDs(9)
		if valid != tc.valid || valid && (len(ids) != 1 || ids[0] != 100) {
			t.Fatalf("value=%s ids=%v valid=%v", tc.value, ids, valid)
		}
	}
}

func TestCompanyCRMRejectsInconsistentPaginationMetadata(t *testing.T) {
	for _, body := range []string{
		`{"_page":2,"_embedded":{"users":[{"id":1}]}}`,
		`{"_total_items":2,"_embedded":{"users":[{"id":1}]}}`,
		`{"_page_count":2,"_embedded":{"users":[{"id":1}]}}`,
		`{"_page_count":41,"_embedded":{"users":[]}}`,
		`{"_total_items":-1,"_embedded":{"users":[]}}`,
		`{"_links":{"next":{}},"_embedded":{"users":[{"id":1}]}}`,
	} {
		c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		rows, err := c.CompanyCRMStaff(t.Context(), uuid.New())
		if err == nil || rows != nil {
			t.Fatalf("body=%s rows=%v err=%v", body, rows, err)
		}
	}
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"_page":1,"_page_count":2,"_total_items":2,"_embedded":{"users":[{"id":1}]},"_links":{"next":{"href":"opaque"}}}`)
			return
		}
		fmt.Fprint(w, `{"_page":2,"_page_count":2,"_total_items":3,"_embedded":{"users":[{"id":2}]}}`)
	})
	if rows, err := c.CompanyCRMStaff(t.Context(), uuid.New()); err == nil || rows != nil {
		t.Fatalf("changing total accepted rows=%v err=%v", rows, err)
	}
}
