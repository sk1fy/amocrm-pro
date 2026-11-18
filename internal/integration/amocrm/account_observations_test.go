package amocrm

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

func TestObservedCRMUsersRequireACompleteRoster(t *testing.T) {
	const userOne = `{"id":9007199254740993,"name":"First","email":"excluded@example.test","rights":{"is_active":true,"is_admin":false}}`
	const userTwo = `{"id":42,"name":"Second","rights":{"is_active":false,"is_admin":true}}`
	const next = `,"_links":{"next":{"href":"https://ignored.example.test/not-an-amo-endpoint"}}`
	page := func(users string, info string) string {
		return `{"_embedded":{"users":[` + users + `]}` + info + `}`
	}
	first := page(userOne, `,"_page":1,"_page_count":2,"_total_items":2`+next)
	second := page(userTwo, `,"_page":2,"_page_count":2,"_total_items":2`)
	tests := []struct {
		name      string
		responses []string
		want      int
	}{
		{"complete", []string{first, second}, 2},
		{"explicit empty", []string{page("", `,"_page":1,"_page_count":0,"_total_items":0`)}, 0},
		{"no content", []string{""}, -1},
		{"missing users", []string{`{"_embedded":{}}`}, -1},
		{"missing admin flag", []string{page(`{"id":1,"name":"Missing","rights":{"is_active":true}}`, "")}, -1},
		{"missing name", []string{page(`{"id":1,"rights":{"is_active":false,"is_admin":false}}`, "")}, -1},
		{"contradictory totals", []string{first, page(userTwo, `,"_page":2,"_page_count":2,"_total_items":3`)}, -1},
		{"missing next page", []string{page(userOne, `,"_page_count":2,"_total_items":2`)}, -1},
		{"duplicates across pages", []string{first, page(userOne, `,"_page":2,"_page_count":2,"_total_items":2`)}, -1},
		{"advertises oversized roster", []string{page(userOne, `,"_page_count":5`+next)}, -1},
		{"partial no content", []string{first, ""}, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := observationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				if err != nil || page < 1 || page > len(tc.responses) || r.URL.Path != "/api/v4/users" || r.URL.Query().Get("limit") != "250" {
					t.Errorf("unexpected request %s", r.URL.RequestURI())
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if tc.responses[page-1] == "" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_, _ = fmt.Fprint(w, tc.responses[page-1])
			})
			users, err := client.ObserveCRMUsers(t.Context(), uuid.New())
			if tc.want < 0 {
				if !errors.Is(err, ErrIncompleteResponse) || users != nil {
					t.Fatalf("incomplete lookup returned users=%+v error=%v", users, err)
				}
				return
			}
			if err != nil || users == nil || len(users) != tc.want {
				t.Fatalf("expected a complete roster of %d, got users=%+v error=%v", tc.want, users, err)
			}
			if tc.want == 2 && (users[0].ID != 9007199254740993 || users[0].Name != "First" || !users[0].IsActive || users[0].IsAdmin ||
				users[1].ID != 42 || users[1].IsActive || !users[1].IsAdmin) {
				t.Fatalf("provider identity or rights changed in observation: %+v", users)
			}
		})
	}
}

func TestObservedAccountIdentityChecksIDAndProviderFields(t *testing.T) {
	client := observationTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/account" || r.URL.Query().Get("with") != "datetime_settings" {
			t.Errorf("unexpected account request: %s", r.URL.RequestURI())
		}
		_, _ = fmt.Fprint(w, `{"id":42,"name":"Acme","subdomain":"new-subdomain","_embedded":{"datetime_settings":{"timezone":"Europe/Moscow"}}}`)
	})
	id := uuid.New()
	identity, err := client.ObserveAccountIdentity(t.Context(), id, 42, "https://old-subdomain.amocrm.ru/")
	if err != nil || identity.CurrentDomain != "new-subdomain.amocrm.ru" || identity.DisplayName != "Acme" ||
		identity.TimeZone == nil || *identity.TimeZone != "Europe/Moscow" {
		t.Fatalf("unexpected confirmed identity %+v, %v", identity, err)
	}
	identity, err = client.ObserveAccountIdentity(t.Context(), id, 43, "old-subdomain.amocrm.ru")
	if !errors.Is(err, ErrIncompleteResponse) || identity.CurrentDomain != "" {
		t.Fatalf("mismatched upstream account returned identity %+v, %v", identity, err)
	}
}

func observationTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = url.Parse
	return client
}
