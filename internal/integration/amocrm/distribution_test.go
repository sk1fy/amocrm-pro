package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestDistributionAccountTimezoneUsesVerifiedAccount(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"valid", `{"id":42,"_embedded":{"datetime_settings":{"timezone":"Europe/Moscow"}}}`, "Europe/Moscow"},
		{"explicit_utc", `{"id":42,"_embedded":{"datetime_settings":{"timezone":"UTC"}}}`, "UTC"},
		{"foreign_account", `{"id":43,"_embedded":{"datetime_settings":{"timezone":"Europe/Moscow"}}}`, ""},
		{"missing_identity", `{"_embedded":{"datetime_settings":{"timezone":"Europe/Moscow"}}}`, ""},
		{"missing_timezone", `{"id":42,"_embedded":{"datetime_settings":{}}}`, ""},
		{"unknown_timezone", `{"id":42,"_embedded":{"datetime_settings":{"timezone":"Unknown/Timezone"}}}`, ""},
		{"server_timezone", `{"id":42,"_embedded":{"datetime_settings":{"timezone":"Local"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/account" || r.URL.Query().Get("with") != "datetime_settings" {
					t.Errorf("unexpected timezone request %s", r.URL)
				}
				fmt.Fprint(w, tc.body)
			})
			tz, err := c.DistributionAccountTimezone(context.Background(), uuid.New(), 42)
			if tc.want == "" {
				if err == nil || tz != "" {
					t.Fatalf("unverified timezone accepted: %q, %v", tz, err)
				}
			} else if err != nil || tz != tc.want {
				t.Fatalf("timezone %q, want %q: %v", tz, tc.want, err)
			}
		})
	}
	invalid := enrichmentClient(t, rejectIfCalled(t))
	if _, err := invalid.DistributionAccountTimezone(context.Background(), uuid.New(), 0); err == nil {
		t.Fatal("missing expected account accepted")
	}
	unavailable := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if tz, err := unavailable.DistributionAccountTimezone(context.Background(), uuid.New(), 42); err == nil || tz != "" {
		t.Fatal("unavailable account defaulted to a timezone", tz, err)
	}
}

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

func TestDistributionLeadsExactBoundedBatch(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
	}{
		{"complete", `{"_embedded":{"leads":[{"id":10,"pipeline_id":20,"status_id":30,"responsible_user_id":1},{"id":11,"pipeline_id":20,"status_id":30,"responsible_user_id":2}]}}`, false},
		{"missing_is_unknown", `{"_embedded":{"leads":[]}}`, false},
		{"foreign", `{"_embedded":{"leads":[{"id":12,"pipeline_id":20,"status_id":30,"responsible_user_id":1}]}}`, true},
		{"duplicate", `{"_embedded":{"leads":[{"id":10,"pipeline_id":20,"status_id":30,"responsible_user_id":1},{"id":10,"pipeline_id":20,"status_id":30,"responsible_user_id":1}]}}`, true},
		{"incomplete", `{"_embedded":{"leads":[{"id":10,"status_id":30,"responsible_user_id":1}]}}`, true},
		{"unexpected_pagination", `{"_links":{"next":{"href":"https://evil.invalid/"}},"_embedded":{"leads":[]}}`, true},
		{"missing_collection", `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.URL.Path != "/api/v4/leads" || q.Get("limit") != "100" || q.Get("page") != "1" || len(q["filter[id][]"]) != 2 || q["filter[id][]"][0] != "10" || q["filter[id][]"][1] != "11" {
					t.Errorf("unbounded request %s", r.URL)
				}
				fmt.Fprint(w, tc.body)
			})
			leads, err := c.DistributionLeads(context.Background(), uuid.New(), []int64{10, 11})
			if (err != nil) != tc.bad || calls != 1 {
				t.Fatalf("%v %v calls=%d", leads, err, calls)
			}
		})
	}
	c := enrichmentClient(t, rejectIfCalled(t))
	for _, ids := range [][]int64{nil, {0}, {10, 10}, make([]int64, 101)} {
		if _, err := c.DistributionLeads(context.Background(), uuid.New(), ids); err == nil {
			t.Fatal("invalid input accepted", ids)
		}
	}
}

func TestDistributionLeadsUnsupportedFilterFallbackOnly(t *testing.T) {
	for _, status := range []int{400, 402, 422, 401, 403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var singles atomic.Int32
			client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v4/leads" {
					w.WriteHeader(status)
					return
				}
				singles.Add(1)
				var id int64
				if _, err := fmt.Sscanf(r.URL.Path, "/api/v4/leads/%d", &id); err != nil {
					t.Error(r.URL)
				}
				fmt.Fprintf(w, `{"id":%d,"pipeline_id":20,"status_id":30,"responsible_user_id":1}`, id)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results, err := client.DistributionLeads(ctx, uuid.New(), []int64{10, 11})
			fallback := status == 400 || status == 402 || status == 422
			if fallback {
				if err != nil || len(results) != 2 || singles.Load() != 2 {
					t.Fatalf("fallback %v %v calls=%d", results, err, singles.Load())
				}
			} else if err == nil || singles.Load() != 0 {
				t.Fatal("authorization/source error fallback", err, singles.Load())
			}
		})
	}
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/leads/10" {
			t.Error("one lead uses filter", r.URL)
		}
		fmt.Fprint(w, `{"id":10,"pipeline_id":20,"status_id":30,"responsible_user_id":1}`)
	})
	if _, err := client.DistributionLeads(context.Background(), uuid.New(), []int64{10}); err != nil {
		t.Fatal(err)
	}
}
