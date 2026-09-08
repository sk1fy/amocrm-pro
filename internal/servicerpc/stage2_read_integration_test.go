package servicerpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// The synthetic upstream data is fed through the production Gateway, collector,
// JSONB store, Activity, protobuf/mTLS and public HTTP serializers. Core widget
// JWT verification and the settings repository have their own existing suites;
// here their boundary inputs are supplied explicitly, with real live policy.
type stage2ReadAPI struct {
	events []amocrm.CRMEvent
	dir    amocrm.AccountDirectory
}

func (a stage2ReadAPI) ListEvents(_ context.Context, _ uuid.UUID, from, to int64, page, limit int) (amocrm.CRMEventPage, error) {
	var events []amocrm.CRMEvent
	for _, event := range a.events {
		if event.CreatedAt >= from && event.CreatedAt <= to {
			events = append(events, event)
		}
	}
	start := min((page-1)*limit, len(events))
	end := min(start+limit, len(events))
	return amocrm.CRMEventPage{Events: events[start:end], HasNext: end < len(events)}, nil
}

func (a stage2ReadAPI) GetDirectory(context.Context, uuid.UUID) (amocrm.AccountDirectory, error) {
	return a.dir, nil
}
func (a stage2ReadAPI) ListNotes(context.Context, uuid.UUID, string, []int64) ([]amocrm.Note, error) {
	return []amocrm.Note{}, nil
}
func (a stage2ReadAPI) ListTasks(context.Context, uuid.UUID, []int64) ([]amocrm.Task, error) {
	return []amocrm.Task{}, nil
}
func (a stage2ReadAPI) ListPipelines(context.Context, uuid.UUID) ([]amocrm.Pipeline, error) {
	return []amocrm.Pipeline{}, nil
}
func (a stage2ReadAPI) ListCustomFields(context.Context, uuid.UUID, string) ([]amocrm.CustomField, error) {
	return []amocrm.CustomField{}, nil
}
func (a stage2ReadAPI) ListEntities(context.Context, uuid.UUID, string, []int64) ([]amocrm.EntityName, error) {
	return []amocrm.EntityName{}, nil
}

func stage2LoadReadFixtures(t *testing.T) (stage2ReadAPI, []serviceapi.Event, int64, int64) {
	t.Helper()
	raw, err := os.ReadFile("../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Period struct {
			From     int64
			To       int64
			Timezone string
		}
		Users []serviceapi.User `json:"reference_users"`
		Cases []struct {
			Input json.RawMessage
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 29 {
		t.Fatalf("expected the complete BASE-02 fixture set, got %d", len(fixture.Cases))
	}
	api := stage2ReadAPI{dir: amocrm.AccountDirectory{Timezone: fixture.Period.Timezone}}
	for _, u := range fixture.Users {
		api.dir.Users = append(api.dir.Users, amocrm.DirectoryUser{ID: u.ID, Name: u.Name, GroupID: u.GroupID, GroupName: u.GroupName})
	}
	var expected []serviceapi.Event
	for _, c := range fixture.Cases {
		var input amocrm.CRMEvent
		var event serviceapi.Event
		if err = json.Unmarshal(c.Input, &input); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(c.Input, &event); err != nil {
			t.Fatal(err)
		}
		api.events = append(api.events, input)
		// Missing B/A has always normalized to []; explicit null stays null.
		if len(event.ValueBefore) == 0 {
			event.ValueBefore = json.RawMessage(`[]`)
		}
		if len(event.ValueAfter) == 0 {
			event.ValueAfter = json.RawMessage(`[]`)
		}
		expected = append(expected, event)
	}
	return api, expected, fixture.Period.From, fixture.Period.To
}

func stage2HTTP(t *testing.T, bridge *activitybridge.Bridge, principal widgetauth.Principal) http.Handler {
	t.Helper()
	router := chi.NewRouter()
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), principal)))
		})
	}
	bridge.RegisterHTTP(router, middleware, middleware)
	return router
}

func stage2Request(t *testing.T, handler http.Handler, target string, status int, into any) {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	if w.Code != status {
		t.Fatalf("GET %s status=%d want=%d body=%s", target, w.Code, status, w.Body.String())
	}
	if status == http.StatusOK && w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("JSON content type missing: %v", w.Header())
	}
	if into != nil {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
}

func stage2JSONMeaning(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func stage2AssertEvent(t *testing.T, want, got serviceapi.Event) {
	t.Helper()
	if !reflect.DeepEqual(stage2JSONMeaning(t, want.ValueBefore), stage2JSONMeaning(t, got.ValueBefore)) ||
		!reflect.DeepEqual(stage2JSONMeaning(t, want.ValueAfter), stage2JSONMeaning(t, got.ValueAfter)) {
		t.Fatalf("event %s changed JSON meaning: want before=%s after=%s; got before=%s after=%s", want.ID, want.ValueBefore, want.ValueAfter, got.ValueBefore, got.ValueAfter)
	}
	want.ValueBefore, want.ValueAfter = nil, nil
	got.ValueBefore, got.ValueAfter = nil, nil
	want = serviceapi.HistoricalEvent(want)
	got = serviceapi.HistoricalEvent(got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("event envelope lost fields: want=%+v got=%+v", want, got)
	}
}

func TestStage2OwnPostgresFixtureHTTPAndMTLSReadParity(t *testing.T) {
	f := newCRMParity(t)
	api, expected, from, to := stage2LoadReadFixtures(t)
	ca := newCA(t)
	ctx := context.Background()
	gw := gateway.New(api, corepolicy.ForCaller(f.policy, serviceapi.GatewayService))
	gwAddress := start(t, ca, &Endpoints{Gateway: gw})
	cfg := crmevents.DefaultConfig()
	cfg.Window = 24 * time.Hour
	cfg.Now = func() time.Time { return time.Unix(to+1, 0).UTC() }
	owner := crmevents.New(f.pool, corepolicy.ForCaller(f.policy, serviceapi.EventsService), dialTest(t, ca, gwAddress, serviceapi.EventsService).Gateway, cfg)
	op, err := owner.Apply(ctx, serviceapi.Command{Auth: f.auth, CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if worked, err := owner.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("owner pass %d worked=%t err=%v", i, worked, err)
		}
	}
	completed, err := owner.Operation(ctx, serviceapi.OperationRequest{Auth: f.auth, OperationID: op.ID})
	if err != nil || completed.State != serviceapi.OperationSucceeded || completed.Inserted != int64(len(expected)) || completed.Deduplicated != int64(len(expected)) {
		t.Fatalf("full fixture ingestion and stable reread: %+v %v", completed, err)
	}
	ownerAddress := startCRMParity(t, ca, owner)
	ownerForActivity := dialTest(t, ca, ownerAddress, serviceapi.ActivityService).CRMEvents
	ownerForCore := dialTest(t, ca, ownerAddress, serviceapi.CoreService).CRMEvents
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	local := product.New(repo, corepolicy.ForCaller(f.policy, serviceapi.ActivityService), owner, gw)
	remoteProduct := product.New(repo, corepolicy.ForCaller(f.policy, serviceapi.ActivityService), ownerForActivity, dialTest(t, ca, gwAddress, serviceapi.ActivityService).Gateway)
	remote := dialTest(t, ca, start(t, ca, &Endpoints{Activity: remoteProduct}), serviceapi.CoreService).Activity
	p := widgetauth.Principal{IntegrationID: f.check.primary.IntegrationID, InstallationID: f.check.primary.InstallationID, AccountID: 42, UserID: 7}
	corePolicy := corepolicy.ForCaller(f.policy, serviceapi.CoreService)
	handlers := map[string]http.Handler{
		"embedded": stage2HTTP(t, activitybridge.New(nil, corePolicy, local, owner), p),
		"remote":   stage2HTTP(t, activitybridge.New(nil, corePolicy, remote, ownerForCore), p),
	}
	// Detail has no directory-author filter. The default panel now also keeps
	// created_by=0 and authors absent from the current directory.
	for mode, handler := range handlers {
		t.Run(mode+"/all-details", func(t *testing.T) {
			for _, want := range expected {
				var got serviceapi.Event
				stage2Request(t, handler, "/api/v1/widget/activity/events/"+url.PathEscape(want.ID), http.StatusOK, &got)
				stage2AssertEvent(t, want, got)
			}
		})
	}
	for _, tc := range []struct {
		name, query string
		accept      func(serviceapi.Event) bool
		compact     bool
		descending  bool
	}{
		{name: "legacy", accept: func(e serviceapi.Event) bool { return true }},
		{name: "type", query: "&types=task_added,task_completed", accept: func(e serviceapi.Event) bool { return e.Type == "task_added" || e.Type == "task_completed" }},
		{name: "family", query: "&type_prefix=task_&order=desc", descending: true, accept: func(e serviceapi.Event) bool { return strings.HasPrefix(e.Type, "task_") }},
		{name: "entity", query: "&entity_type=task&entity_ids=41001", accept: func(e serviceapi.Event) bool { return e.EntityType == "task" && e.EntityID == 41001 }},
		{name: "unknown-type", query: "&types=synthetic_future_event_v9", accept: func(e serviceapi.Event) bool { return e.Type == "synthetic_future_event_v9" }},
		{name: "absent-directory-author", query: "&user_ids=71999", accept: func(e serviceapi.Event) bool { return e.CreatedBy == 71999 }},
		{name: "compact", query: "&compact=true&order=desc", compact: true, descending: true, accept: func(e serviceapi.Event) bool { return true }},
		{name: "empty-user", query: "&user_ids=71002", accept: func(e serviceapi.Event) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []serviceapi.Event
			for _, event := range expected {
				if tc.accept(event) {
					want = append(want, event)
				}
			}
			sort.Slice(want, func(i, j int) bool {
				less := want[i].CreatedAt < want[j].CreatedAt || (want[i].CreatedAt == want[j].CreatedAt && want[i].ID < want[j].ID)
				if tc.descending {
					return !less
				}
				return less
			})
			var previous []serviceapi.Event
			for mode, handler := range handlers {
				target := fmt.Sprintf("/api/v1/widget/activity/panel?from=%d&to=%d&limit=3%s", from, to, tc.query)
				var all []serviceapi.Event
				var summary []serviceapi.UserSummary
				for page := 0; ; page++ {
					if page > len(expected) {
						t.Fatal("cursor failed to terminate")
					}
					var result serviceapi.Panel
					stage2Request(t, handler, target, http.StatusOK, &result)
					if result.Data.ReadVersion != serviceapi.PresentationReadVersion || result.Data.PayloadsOmitted != tc.compact {
						t.Fatalf("%s lost read/projection metadata %+v", mode, result.Data)
					}
					if page == 0 {
						summary = result.Data.Summaries
					} else if !reflect.DeepEqual(summary, result.Data.Summaries) {
						t.Fatal("whole-period summary changed with page")
					}
					all = append(all, result.Data.Events...)
					if result.Data.NextCursor == "" {
						break
					}
					parsed, _ := url.Parse(target)
					query := parsed.Query()
					query.Set("cursor", result.Data.NextCursor)
					parsed.RawQuery = query.Encode()
					target = parsed.String()
				}
				if len(all) != len(want) {
					t.Fatalf("%s got %d events want %d", mode, len(all), len(want))
				}
				var count int64
				for _, entry := range summary {
					count += entry.UniqueEvents
				}
				if count != int64(len(want)) {
					t.Fatalf("%s summary=%d want %d", mode, count, len(want))
				}
				for i, event := range all {
					if tc.compact {
						if event.ID != want[i].ID || string(event.ValueBefore) != "null" || string(event.ValueAfter) != "null" {
							t.Fatalf("compact page not explicitly omitted: %+v", event)
						}
					} else {
						stage2AssertEvent(t, want[i], event)
					}
				}
				if previous != nil && !reflect.DeepEqual(previous, all) {
					t.Fatal("embedded/remote HTTP data differ")
				}
				previous = all
			}
		})
	}
	for mode, handler := range handlers {
		t.Run(mode+"/errors", func(t *testing.T) {
			stage2Request(t, handler, "/api/v1/widget/activity/events/does-not-exist", http.StatusNotFound, nil)
			stage2Request(t, handler, fmt.Sprintf("/api/v1/widget/activity/panel?from=%d&to=%d&types=task_added,task_added", from, to), http.StatusBadRequest, nil)
			f.check.disabled.Store(true)
			stage2Request(t, handler, "/api/v1/widget/activity/events/"+expected[0].ID, http.StatusForbidden, nil)
			f.check.disabled.Store(false)
		})
	}
	other := p
	other.IntegrationID, other.InstallationID = f.check.other.IntegrationID, f.check.other.InstallationID
	for _, dependency := range []serviceapi.CRMEvents{owner, ownerForCore} {
		foreign := stage2HTTP(t, activitybridge.New(nil, corePolicy, local, dependency), other)
		stage2Request(t, foreign, "/api/v1/widget/activity/events/"+expected[0].ID, http.StatusNotFound, nil)
	}
}
