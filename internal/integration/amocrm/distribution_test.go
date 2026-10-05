package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"testing"
)

func TestDistributionUsersPaginationAndIncompleteSources(t *testing.T) {
	for _, kind := range []string{"complete", "duplicate", "missing_active", "empty_next", "too_many_pages"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/v4/users" || r.URL.Query().Get("page") == "" {
					t.Fatal("unsafe paginationURL", r.URL)
				}
				id := calls
				next := `"_links":{"next":{"href":"https://attacker.invalid/ignored"}},`
				if calls == 2 && kind != "too_many_pages" {
					next = ""
				}
				active := `,"rights":{"is_active":true}`
				if kind == "missing_active" {
					active = ""
				}
				if kind == "duplicate" {
					id = 1
				}
				if kind == "empty_next" {
					fmt.Fprint(w, `{"_links":{"next":{"href":"ignored"}},"_embedded":{"users":[]}}`)
					return
				}
				fmt.Fprintf(w, `{%s"_embedded":{"users":[{"id":%d,"name":"User"%s}]}}`, next, id, active)
			})
			users, e := c.DistributionUsers(context.Background(), uuid.New())
			if kind == "complete" {
				if e != nil || len(users) != 2 {
					t.Fatal(users, e)
				}
			} else if e == nil {
				t.Fatal("incomplete sources accepted")
			}
		})
	}
}
func TestDistributionPipelineAndSubscriberPages(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		next := ""
		page := r.URL.Query().Get("page")
		if page == "1" {
			next = `"_links":{"next":{"href":"https://attacker.invalid/ignored"}},`
		}
		if r.URL.Path == "/api/v4/leads/pipelines" {
			fmt.Fprintf(w, `{%s"_embedded":{"pipelines":[{"id":%s,"name":"Pipeline","_embedded":{"statuses":[{"id":10,"name":"Stage"}]}}]}}`, next, page)
			return
		}
		if r.URL.Path == "/api/v4/leads/9/subscriptions" {
			fmt.Fprintf(w, `{%s"_embedded":{"subscriptions":[{"subscriber_id":%s,"type":"user"}]}}`, next, page)
			return
		}
		t.Fatal("unbounded path", r.URL)
	})
	ps, e := c.DistributionPipelines(context.Background(), uuid.New())
	if e != nil || len(ps) != 2 {
		t.Fatal(ps, e)
	}
	ss, e := c.DistributionSubscriptions(context.Background(), uuid.New(), 9)
	if e != nil || len(ss) != 2 {
		t.Fatal(ss, e)
	}
}
func TestDistributionAuthorizationIdentityIsVerified(t *testing.T) {
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":2,"rights":{"is_active":true}}`) })
	if _, e := c.DistributionUser(context.Background(), uuid.New(), 1); e == nil {
		t.Fatal("foreignuser response accepted")
	}
	if _, e := c.DistributionLead(context.Background(), uuid.New(), 1); e == nil {
		t.Fatal("incompletelead accepted")
	}
}
func TestDistributionMissingCollectionsFailClosed(t *testing.T) {
	for _, raw := range []string{`{}`, `{"_embedded":{}}`, `{"_embedded":{"users":null,"pipelines":null,"subscriptions":null}}`} {
		c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) })
		if _, e := c.DistributionUsers(context.Background(), uuid.New()); e == nil {
			t.Fatal("missingusers accepted")
		}
		if _, e := c.DistributionPipelines(context.Background(), uuid.New()); e == nil {
			t.Fatal("missingpipelines accepted")
		}
		if _, e := c.DistributionSubscriptions(context.Background(), uuid.New(), 9); e == nil {
			t.Fatal("missingsubscriptions accepted")
		}
	}
	c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	s, e := c.DistributionSubscriptions(context.Background(), uuid.New(), 9)
	if e != nil || s == nil || len(s) != 0 {
		t.Fatal("documented204", s, e)
	}
}
