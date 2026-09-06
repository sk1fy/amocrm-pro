package amocrm

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestEventsUsesBoundedFixedRangeAndDoesNotFollowNextURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/events" || q.Get("limit") != "100" || q.Get("page") != "3" || q.Get("filter[created_at][from]") != "1000" || q.Get("filter[created_at][to]") != "2000" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"events":[{"id":"event-1","created_at":1000,"created_by":7,"type":"lead_added","entity_id":8,"entity_type":"lead","value_before":[],"value_after":[{"x":1}]}]}}`))
	}))
	defer server.Close()
	c := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	c.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	result, err := c.ListEvents(context.Background(), uuid.New(), 1000, 2000, 3, 100)
	if err != nil || len(result.Events) != 1 || !result.HasNext || calls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
	if _, err := c.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 250); err == nil {
		t.Fatal("accepted PHP page size250")
	}
	if calls != 1 {
		t.Fatal("invalid request reached API")
	}
}
func TestEventsEmptyWindowAndRateLimit(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
			c.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
			result, err := c.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100)
			if status == http.StatusNoContent {
				if err != nil || len(result.Events) != 0 || result.HasNext {
					t.Fatalf("empty=%+v err=%v", result, err)
				}
			} else {
				var api *APIError
				if !errors.As(err, &api) || !api.Retryable || api.RetryAfter != 9*time.Second {
					t.Fatalf("rate limit=%v", err)
				}
			}
		})
	}
}
func TestDirectoryOnlyUsesBoundedApprovedEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/account":
			_, _ = w.Write([]byte(`{"_embedded":{"users_groups":[{"id":3,"name":"Sales"}],"datetime_settings":{"timezone":"Europe/Moscow"}}}`))
		case "/api/v4/users":
			if r.URL.Query().Get("limit") != "250" {
				t.Error("unbounded page")
			}
			_, _ = w.Write([]byte(`{"_embedded":{"users":[{"id":7,"name":"Alice","email":"private@example.com","rights":{"group_id":3}}]}}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	c.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	directory, err := c.GetDirectory(context.Background(), uuid.New())
	if err != nil || directory.Timezone != "Europe/Moscow" || len(directory.Users) != 1 || directory.Users[0].GroupName != "Sales" {
		t.Fatalf("directory=%+v err=%v", directory, err)
	}
}
