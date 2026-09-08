package activitybridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func TestStage2HTTPQueryDecodesBoundedReadOptions(t *testing.T) {
	q, err := decodeQuery(httptest.NewRequest(http.MethodGet, "/?from=100&to=200&types=task_added,task_completed&type_prefix=custom_&entity_type=task&entity_ids=41,42&order=desc&compact=true&user_ids=7&limit=12", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := serviceapi.Query{From: 100, To: 200, UserIDs: []int64{7}, Limit: 12, Types: []string{"task_added", "task_completed"}, TypePrefix: "custom_", EntityType: "task", EntityIDs: []int64{41, 42}, Order: "desc", Compact: true}
	if !reflect.DeepEqual(want, q) {
		t.Fatalf("HTTP options lost: want=%+v got=%+v", want, q)
	}
	presented, err := decodeQuery(httptest.NewRequest(http.MethodGet, "/?from=100&to=200&categories=tasks,calls&include_unknown_authors=true&buckets=auto&group_id=72001", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(presented.Categories, []string{"tasks", "calls"}) || !presented.IncludeUnknownAuthors || presented.Buckets != "auto" || presented.GroupID != 72001 {
		t.Fatalf("presentation options lost: %+v", presented)
	}
	legacy, err := decodeQuery(httptest.NewRequest(http.MethodGet, "/?from=100&to=200", nil))
	if err != nil || !reflect.DeepEqual(legacy, serviceapi.Query{From: 100, To: 200, Limit: 100}) {
		t.Fatalf("legacy defaults changed: %+v %v", legacy, err)
	}
	for _, suffix := range []string{
		"types=task_added,task_added", "types=task_added&types=task_completed",
		"entity_type=task&entity_ids=41,41", "entity_type=task&entity_ids=-1", "entity_type=task&entity_ids=9223372036854775808",
		"order=random", "compact=perhaps", "compact=true&compact=false",
		"type_prefix=task_&type_prefix=lead_", "installation_id=other",
		"types=task%ZZ", "type_prefix=task_;x", "compact=tr%ZZue", "compact=true;x",
		"categories=tasks,tasks", "categories=future", "buckets=shift", "group_id=0", "include_unknown_authors=yes",
	} {
		if _, err := decodeQuery(httptest.NewRequest(http.MethodGet, "/?from=100&to=200&"+suffix, nil)); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("accepted invalid public query %s: %v", suffix, err)
		}
	}
}

type stage2OldActivity struct{ serviceapi.Activity }

func (stage2OldActivity) Panel(context.Context, serviceapi.Query) (serviceapi.Panel, error) {
	// An old peer may ignore request fields and send a valid legacy result.
	return serviceapi.Panel{Data: serviceapi.QueryResult{Events: []serviceapi.Event{}}}, nil
}

func TestStage2BridgeRejectsUnprovenReadCapabilities(t *testing.T) {
	b := New(nil, &admissionPolicy{}, stage2OldActivity{}, nil)
	p := widgetauth.Principal{IntegrationID: uuid.New(), InstallationID: uuid.New(), UserID: 7}
	for _, suffix := range []string{"types=task_added", "type_prefix=task_", "entity_type=task", "entity_type=task&entity_ids=41", "order=desc", "compact=true", "cursor=versioned-cursor", "categories=tasks", "include_unknown_authors=true", "buckets=hour", "group_id=9"} {
		r := httptest.NewRequest(http.MethodGet, "/?from=100&to=200&"+suffix, nil)
		r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		b.PanelHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("old Activity silently accepted %s: status=%d body=%s", suffix, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/?from=100&to=200", nil)
	r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	b.PanelHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy read must remain compatible: %d %s", w.Code, w.Body.String())
	}
}

func TestStage2DetailRouteRequiresReadMiddlewareAndAuthentication(t *testing.T) {
	b := New(nil, &admissionPolicy{}, nil, nil)
	router := chi.NewRouter()
	reads, commands := 0, 0
	read := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reads++
			next.ServeHTTP(w, r)
		})
	}
	command := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			commands++
			next.ServeHTTP(w, r)
		})
	}
	b.RegisterHTTP(router, read, command)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/widget/activity/events/synthetic-task_added", nil))
	if w.Code != http.StatusUnauthorized || reads != 1 || commands != 0 {
		t.Fatalf("detail route skipped read admission: status=%d reads=%d commands=%d", w.Code, reads, commands)
	}
}
