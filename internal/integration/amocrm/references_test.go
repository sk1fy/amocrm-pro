package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/crmreferences"
)

func referenceFixture(w http.ResponseWriter, r *http.Request, mode string) bool {
	switch r.URL.Path {
	case "/api/v4/account":
		fmt.Fprintf(w, `{"id":123,"customers_mode":%q,"_embedded":{"users_groups":[{"id":10,"name":"Empty group"}]}}`, mode)
	case "/api/v4/users":
		fmt.Fprint(w, `{"_embedded":{"users":[]}}`)
	case "/api/v4/leads/pipelines":
		fmt.Fprint(w, `{"_embedded":{"pipelines":[{"id":7,"name":"Покупатели","sort":5,"_embedded":{"statuses":[{"id":142,"name":"Done","sort":20,"color":"#fff","pipeline_id":7,"account_id":123},{"id":143,"name":"Lost","sort":30},{"id":8,"name":"Open","sort":10}]}}]}}`)
	default:
		return false
	}
	return true
}
func TestCRMReferencesCustomersAndAuthoritativeGroups(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		status       int
		availability string
	}{{"disabled", 200, "unsupported"}, {"unavailable", 200, "unsupported"}, {"unknown-mode", 200, "unknown"}, {"periodicity", 403, "unknown"}, {"periodicity", 401, "unknown"}, {"segments", 200, "supported"}, {"dynamic", 503, "unknown"}} {
		t.Run(tc.mode+strconv.Itoa(tc.status), func(t *testing.T) {
			customerCalls := 0
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if referenceFixture(w, r, tc.mode) {
					return
				}
				if r.URL.Path != "/api/v4/customers/statuses" {
					t.Error("unexpected route", r.URL)
					return
				}
				customerCalls++
				if tc.status != 200 {
					w.WriteHeader(tc.status)
					return
				}
				fmt.Fprint(w, `{"_embedded":{"statuses":[{"id":20,"name":"Periodic","sort":5,"color":"#fff","account_id":123}]}}`)
			})
			d, err := c.CRMReferences(context.Background(), uuid.New(), 123)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Groups) != 1 || d.Groups[0].ID != "10" || len(d.Users) != 0 {
				t.Fatal("empty groups lost", d.Groups)
			}
			if d.Customers.Availability != tc.availability {
				t.Fatal(d.Customers)
			}
			if tc.availability == "unsupported" && customerCalls != 0 {
				t.Fatal("unsupported queried")
			}
			if tc.availability == "supported" && len(d.Customers.Statuses) != 1 {
				t.Fatal("statuses missing")
			}
			if tc.availability == "unknown" && d.Customers.ErrorCode == nil {
				t.Fatal("unknown reason omitted")
			}
			p := d.Pipelines[0]
			if p.Statuses[0].Kind != "won" || p.Statuses[1].Kind != "lost" || p.Statuses[2].Kind != "open" || p.Sort != 5 {
				t.Fatal("wrong stage semantics", p)
			}
		})
	}
}
func TestCRMReferencesFullUsersAndFailedSecondPage(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/users" {
					referenceFixture(w, r, "disabled")
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 2 && fail {
					w.WriteHeader(503)
					return
				}
				start, count := 1, 60
				if page == 2 {
					start, count = 61, 50
				}
				users := []map[string]any{}
				for i := start; i < start+count; i++ {
					users = append(users, map[string]any{"id": i, "name": "User", "rights": map[string]any{"is_active": true}})
				}
				response := map[string]any{"_embedded": map[string]any{"users": users}}
				if page == 1 {
					response["_links"] = map[string]any{"next": map[string]any{"href": "https://untrusted.invalid/never-follow"}}
				}
				_ = json.NewEncoder(w).Encode(response)
			})
			d, err := c.CRMReferences(context.Background(), uuid.New(), 123)
			if fail {
				if err == nil {
					t.Fatal("partial reference snapshot accepted")
				}
				return
			}
			if err != nil || len(d.Users) != 110 {
				t.Fatal("truncated users", len(d.Users), err)
			}
		})
	}
}
func TestCRMReferencesRejectsIncompleteIdentityAndPipelineLinks(t *testing.T) {
	for _, kind := range []string{"foreign_account", "missing_groups", "foreign_pipeline", "duplicate_pipeline", "too_many_groups"} {
		t.Run(kind, func(t *testing.T) {
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if kind == "foreign_account" && r.URL.Path == "/api/v4/account" {
					fmt.Fprint(w, `{"id":999,"_embedded":{"users_groups":[]}}`)
					return
				}
				if kind == "missing_groups" && r.URL.Path == "/api/v4/account" {
					fmt.Fprint(w, `{"id":123}`)
					return
				}
				if kind == "too_many_groups" && r.URL.Path == "/api/v4/account" {
					groups := make([]map[string]any, 1001)
					for i := range groups {
						groups[i] = map[string]any{"id": i + 1, "name": "group"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "_embedded": map[string]any{"users_groups": groups}})
					return
				}
				if r.URL.Path == "/api/v4/leads/pipelines" {
					if kind == "foreign_pipeline" {
						fmt.Fprint(w, `{"_embedded":{"pipelines":[{"id":2,"_embedded":{"statuses":[{"id":8,"pipeline_id":999}]}}]}}`)
					} else {
						fmt.Fprint(w, `{"_embedded":{"pipelines":[{"id":2,"_embedded":{"statuses":[]}},{"id":2,"_embedded":{"statuses":[]}}]}}`)
					}
					return
				}
				referenceFixture(w, r, "disabled")
			})
			_, err := c.CRMReferences(context.Background(), uuid.New(), 123)
			if err == nil {
				t.Fatal("invalid source accepted")
			}
			if kind == "too_many_groups" && !errors.Is(err, crmreferences.ErrTooLarge) {
				t.Fatal(err)
			}
		})
	}
}

func TestCRMReferencesThousandUsersAndOverflow(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(strconv.FormatBool(overflow), func(t *testing.T) {
			pages := 0
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/users" {
					referenceFixture(w, r, "disabled")
					return
				}
				pages++
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				users := []map[string]any{}
				for n := 1; n <= 250; n++ {
					users = append(users, map[string]any{"id": (page-1)*250 + n, "name": "User", "rights": map[string]any{"is_active": true}})
				}
				response := map[string]any{"_embedded": map[string]any{"users": users}}
				if page < 4 || overflow {
					response["_links"] = map[string]any{"next": map[string]any{"href": "next-page"}}
				}
				_ = json.NewEncoder(w).Encode(response)
			})
			d, err := c.CRMReferences(context.Background(), uuid.New(), 123)
			if overflow {
				if err == nil {
					t.Fatal("more than 1000 users truncated as complete")
				}
			} else if err != nil || len(d.Users) != 1000 {
				t.Fatal("1000 users not read completely", len(d.Users), err)
			}
			if pages != 4 {
				t.Fatal("reader exceeded bounded pages", pages)
			}
		})
	}
}
