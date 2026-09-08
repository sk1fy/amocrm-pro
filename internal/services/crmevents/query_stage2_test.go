package crmevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestEventCursorBindsNormalizedQueryAndScope(t *testing.T) {
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}}
	q := serviceapi.Query{From: 100, To: 200, UserIDs: []int64{9, 7, 9}, Types: []string{"task_added", "future_event", "task_added"}, TypePrefix: "call_", EntityType: "lead", EntityIDs: []int64{2, 1, 2}, Compact: true, Limit: 1}
	q.Cursor = encodeCursor(q, p, serviceapi.Event{ID: "event-1", CreatedAt: 150})
	equivalent := q
	equivalent.UserIDs = []int64{7, 9}
	equivalent.Types = []string{"future_event", "task_added"}
	equivalent.EntityIDs = []int64{1, 2}
	equivalent.Order = "asc"
	equivalent.Limit = 100
	equivalent.Auth.Token = "renewed-capability"
	if c, err := decodeCursor(equivalent, p); err != nil || c.ID != "event-1" {
		t.Fatalf("equivalent query rejected: %+v %v", c, err)
	}
	if !reflect.DeepEqual(q.UserIDs, []int64{9, 7, 9}) || !reflect.DeepEqual(q.Types, []string{"task_added", "future_event", "task_added"}) || !reflect.DeepEqual(q.EntityIDs, []int64{2, 1, 2}) {
		t.Fatal("cursor normalization mutated caller slices")
	}
	for name, change := range map[string]func(*serviceapi.Query){
		"from":        func(q *serviceapi.Query) { q.From-- },
		"to":          func(q *serviceapi.Query) { q.To++ },
		"users":       func(q *serviceapi.Query) { q.UserIDs = []int64{7} },
		"types":       func(q *serviceapi.Query) { q.Types = []string{"task_added"} },
		"prefix":      func(q *serviceapi.Query) { q.TypePrefix = "task_" },
		"entity type": func(q *serviceapi.Query) { q.EntityType = "contact" },
		"entity ids":  func(q *serviceapi.Query) { q.EntityIDs = []int64{1} },
		"order":       func(q *serviceapi.Query) { q.Order = "desc" },
		"compact":     func(q *serviceapi.Query) { q.Compact = false },
		"categories":  func(q *serviceapi.Query) { q.Categories = []string{serviceapi.CategoryTasks} },
		"unknown":     func(q *serviceapi.Query) { q.IncludeUnknownAuthors = true },
		"buckets":     func(q *serviceapi.Query) { q.Buckets = serviceapi.BucketHour; q.Timezone = "Europe/Moscow" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := q
			change(&changed)
			if _, err := decodeCursor(changed, p); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
				t.Fatalf("changed query accepted: %v", err)
			}
		})
	}
	for _, scope := range []serviceapi.Scope{{InstallationID: uuid.New(), IntegrationID: p.IntegrationID}, {InstallationID: p.InstallationID, IntegrationID: uuid.New()}} {
		other := p
		other.Scope = scope
		if _, err := decodeCursor(q, other); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("cross-scope cursor accepted: %v", err)
		}
	}
	legacy := q
	legacy.Cursor = base64.RawURLEncoding.EncodeToString([]byte(`{"at":150,"id":"event-1"}`))
	if _, err := decodeCursor(legacy, p); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("legacy cursor did not request restart: %v", err)
	}
	empty := serviceapi.Query{From: 100, To: 200}
	explicit := empty
	explicit.UserIDs, explicit.Types, explicit.EntityIDs, explicit.Order = []int64{}, []string{}, []int64{}, "asc"
	if queryFingerprint(empty, p) != queryFingerprint(explicit, p) {
		t.Fatal("empty/default filters have different fingerprints")
	}
}

type stage2DetailRepository struct {
	Repository
	event serviceapi.Event
	calls int
}

func (r *stage2DetailRepository) GetEvent(context.Context, serviceapi.Principal, string) (serviceapi.Event, error) {
	r.calls++
	return r.event, nil
}

type stage2ReadPolicy struct {
	testPolicy
	actions []string
	err     error
}

func (p *stage2ReadPolicy) Validate(_ context.Context, _ serviceapi.Auth, audience, action string) (serviceapi.Principal, error) {
	p.actions = append(p.actions, audience+":"+action)
	return p.principal, p.err
}

func TestEventDetailChecksLivePolicyAndResponseBudget(t *testing.T) {
	repo := &stage2DetailRepository{event: serviceapi.Event{ID: "event-1", ValueAfter: json.RawMessage(`[{"text":"result"}]`)}}
	policy := &stage2ReadPolicy{}
	s := NewWithRepository(repo, policy, nil, DefaultConfig())
	req := serviceapi.EventRequest{EventID: "event-1"}
	if event, err := s.GetEvent(context.Background(), req); err != nil || event.ID != "event-1" {
		t.Fatalf("detail %+v %v", event, err)
	}
	policy.err = serviceapi.Fail(serviceapi.PermissionDenied, "revoked")
	if _, err := s.GetEvent(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || repo.calls != 1 {
		t.Fatalf("revoked live policy reached repository: calls=%d err=%v", repo.calls, err)
	}
	for _, action := range policy.actions {
		if action != serviceapi.EventsService+":"+serviceapi.ActionRead {
			t.Fatalf("wrong detail authorization %q", action)
		}
	}
	policy.err = nil
	if _, err := s.GetEvent(context.Background(), serviceapi.EventRequest{}); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument || repo.calls != 1 {
		t.Fatalf("invalid ID reached repository: calls=%d err=%v", repo.calls, err)
	}
	repo.event.ValueAfter = json.RawMessage(`"` + strings.Repeat("x", serviceapi.MaxResponseBytes) + `"`)
	if _, err := s.GetEvent(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
		t.Fatalf("oversized detail accepted: %v", err)
	}
}
