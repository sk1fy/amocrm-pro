package amocrm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEventsFixtureDecodingPreservesTypedEnvelopeAndNestedJSON(t *testing.T) {
	data, err := os.ReadFile("../../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Period struct{ From, To int64 }
		Cases  []struct {
			ID    string          `json:"case_id"`
			Input json.RawMessage `json:"input"`
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 29 {
		t.Fatalf("fixture changed: review coverage of all %d cases", len(fixture.Cases))
	}
	inputs := make([]json.RawMessage, 0, len(fixture.Cases))
	for _, entry := range fixture.Cases {
		inputs = append(inputs, entry.Input)
	}
	body, err := json.Marshal(map[string]any{"_embedded": map[string]any{"events": inputs}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	page, err := client.ListEvents(context.Background(), uuid.New(), fixture.Period.From, fixture.Period.To, 1, 100)
	if err != nil || len(page.Events) != len(fixture.Cases) || page.HasNext {
		t.Fatalf("fixture page count=%d has_next=%t err=%v", len(page.Events), page.HasNext, err)
	}
	for i, entry := range fixture.Cases {
		t.Run(entry.ID, func(t *testing.T) {
			var expected CRMEvent
			if err := json.Unmarshal(entry.Input, &expected); err != nil {
				t.Fatal(err)
			}
			got := page.Events[i]
			if got.ID != expected.ID || got.CreatedAt != expected.CreatedAt || got.CreatedBy != expected.CreatedBy || got.Type != expected.Type || got.EntityID != expected.EntityID || got.EntityType != expected.EntityType || got.LinkedTalkContactID != expected.LinkedTalkContactID {
				t.Fatalf("typed envelope changed: got=%+v expected=%+v", got, expected)
			}
			// RawMessage retains numbers, arrays, explicit null, and missing fields.
			for field, pair := range map[string][2]json.RawMessage{"before": {expected.ValueBefore, got.ValueBefore}, "after": {expected.ValueAfter, got.ValueAfter}} {
				if !bytes.Equal(pair[0], pair[1]) {
					var want, actual any
					for j, payload := range pair {
						decoder := json.NewDecoder(bytes.NewReader(payload))
						decoder.UseNumber()
						var value any
						if err := decoder.Decode(&value); err != nil {
							t.Fatalf("%s decode: %v", field, err)
						}
						if j == 0 {
							want = value
						} else {
							actual = value
						}
					}
					if !reflect.DeepEqual(want, actual) {
						t.Fatalf("%s payload changed", field)
					}
				}
			}
		})
	}
	for _, event := range page.Events {
		if event.ID == "synthetic-chat_reference" && event.LinkedTalkContactID != 32001 {
			t.Fatal("chat contact reference was dropped")
		}
	}
}

func TestEventsPayloadBoundsRejectWholePage(t *testing.T) {
	// Length includes the JSON quotes; the limit is bytes, not characters.
	exact := `"` + strings.Repeat("x", 32766) + `"`
	over := `"` + strings.Repeat("x", 32767) + `"`
	for _, tc := range []struct {
		name, before, after, linked string
		accept                      bool
	}{
		{"exact_both", exact, exact, fmt.Sprint(int64(math.MaxInt64)), true},
		{"over_before", over, `[]`, "0", false},
		{"over_after", `[]`, over, "0", false},
		{"negative_contact", `[]`, `[]`, "-1", false},
		{"overflow_contact", `[]`, `[]`, "9223372036854775808", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"_embedded":{"events":[{"id":"valid","created_at":1000},{"id":"bounded","created_at":1001,"linked_talk_contact_id":%s,"value_before":%s,"value_after":%s}]}}`, tc.linked, tc.before, tc.after)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
			client.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
			page, err := client.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100)
			if tc.accept {
				if err != nil || len(page.Events) != 2 || len(page.Events[1].ValueBefore) != 32768 || len(page.Events[1].ValueAfter) != 32768 || page.Events[1].LinkedTalkContactID != math.MaxInt64 {
					t.Fatalf("exact bounds rejected or truncated: count=%d err=%v", len(page.Events), err)
				}
			} else if err == nil || len(page.Events) != 0 || page.HasNext {
				t.Fatalf("invalid event must reject entire page: count=%d has_next=%t err=%v", len(page.Events), page.HasNext, err)
			}
		})
	}
}

func TestEventsResponseLimitRejectsPageOfIndividuallyBoundedEvents(t *testing.T) {
	payload := `"` + strings.Repeat("x", 32766) + `"`
	events := make([]string, 65)
	for i := range events {
		events[i] = fmt.Sprintf(`{"id":"event-%d","created_at":1000,"value_before":%s,"value_after":%s}`, i, payload, payload)
	}
	body := `{"_embedded":{"events":[` + strings.Join(events, ",") + `]}}`
	if len(body) <= maxAPIResponseBody {
		t.Fatal("test did not exceed body limit")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer server.Close()
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	page, err := client.ListEvents(context.Background(), uuid.New(), 1000, 2000, 1, 100)
	if err == nil || len(page.Events) != 0 || page.HasNext {
		t.Fatalf("oversize response accepted: count=%d err=%v", len(page.Events), err)
	}
}

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
