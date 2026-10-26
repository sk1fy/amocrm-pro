package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProblemSnapshotFollowsPagesWithoutFollowingExternalNextURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/api/v4/leads" {
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `{"_embedded":{"leads":[{"id":1}]},"_links":{"next":{"href":"https://untrusted.invalid"}}}`)
			} else {
				fmt.Fprint(w, `{"_embedded":{"leads":[{"id":2}]}}`)
			}
		} else {
			if r.URL.Query().Get("filter[is_completed]") != "0" {
				t.Error("missing active task filter")
			}
			fmt.Fprint(w, `{"_embedded":{"tasks":[{"id":3}]}}`)
		}
	}))
	defer server.Close()
	c := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	c.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	id := uuid.New()
	first, err := c.ReadProblemPage(context.Background(), id, "leads", 1)
	if err != nil || !first.HasNext || len(first.Leads) != 1 {
		t.Fatalf("first page %+v %v", first, err)
	}
	second, err := c.ReadProblemPage(context.Background(), id, "leads", 2)
	if err != nil || second.HasNext || len(second.Leads) != 1 {
		t.Fatalf("second page %+v %v", second, err)
	}
	tasks, err := c.ReadProblemPage(context.Background(), id, "tasks", 1)
	if err != nil || len(tasks.Tasks) != 1 || calls != 3 {
		t.Fatalf("tasks %+v %v calls %d", tasks, err, calls)
	}
}
func TestProblemSnapshotFailureDoesNotPublishPartialLeads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/leads" {
			fmt.Fprint(w, `{"_embedded":{"leads":[{"id":1}]}}`)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	c := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	c.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	got, err := c.ReadProblemPage(context.Background(), uuid.New(), "tasks", 1)
	if err == nil || len(got.Leads) != 0 {
		t.Fatalf("partial snapshot published %+v %v", got, err)
	}
}
