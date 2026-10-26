package activity

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestActivityRatingLegacySharedFixtures(t *testing.T) {
	bytes, err := os.ReadFile("../../../docs/fixtures/activity-rating-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, DayKey, TimeZone   string
		StartMinutes, EndMinutes int
		CutoffUnix               *int64
		Events                   []struct {
			Type      string
			CreatedAt int64
			Call      *struct{ DurationSeconds *float64 }
		}
		ExpectedDashboard, ExpectedProfile float64
	}
	if err := json.Unmarshal(bytes, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			loc, err := time.LoadLocation(c.TimeZone)
			if err != nil {
				t.Fatal(err)
			}
			events := []ratingEvent{}
			for _, e := range c.Events {
				v := ratingEvent{At: e.CreatedAt, Type: e.Type}
				if e.Call != nil {
					v.Duration = e.Call.DurationSeconds
				}
				events = append(events, v)
			}
			if got := dashboardRating(events, c.DayKey, loc, c.StartMinutes, c.EndMinutes, c.CutoffUnix); got != c.ExpectedDashboard {
				t.Fatalf("dashboard got%v want%v", got, c.ExpectedDashboard)
			}
			series := profileRatingSeries(events, c.DayKey, loc, c.StartMinutes, c.EndMinutes)
			if len(series) != 1441 {
				t.Fatal("profile series shape")
			}
			minute := 1440
			if c.CutoffUnix != nil {
				cut := time.Unix(*c.CutoffUnix, 0).In(loc)
				if cut.Format("2006-01-02") == c.DayKey {
					minute = cut.Hour()*60 + cut.Minute()
				}
			}
			if got := series[minute]; got != c.ExpectedProfile {
				t.Fatalf("profile got%v want%v", got, c.ExpectedProfile)
			}
		})
	}
}
func TestRatingPipelineAndCanonicalCallFacts(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want bool
	}{{`[{"lead_status":{"pipeline_id":10}}]`, true}, {`[{"lead_status":{"pipeline_id":11}}]`, false}, {`[]`, true}, {`[{"options":{"pipeline":{"id":"10"}}}]`, true}, {`[{"pipeline":{"id":11}},{"pipeline":{"id":10}}]`, true}} {
		if got := ratingPipelineMatch(serviceapi.Event{ValueAfter: json.RawMessage(c.raw)}, []int64{10}); got != c.want {
			t.Fatalf("pipeline%v=%v", c.raw, got)
		}
	}
	zero := int64(0)
	for _, c := range []struct {
		event serviceapi.Event
		want  *float64
	}{
		{serviceapi.Event{Type: "incoming_call", CallFacts: &serviceapi.CallFacts{Duration: &zero}, ValueAfter: json.RawMessage(`[{"note":{"params":{"duration":120}}}]`)}, number(0)},
		{serviceapi.Event{Type: "incoming_call", CallFacts: &serviceapi.CallFacts{}, ValueAfter: json.RawMessage(`[{"note":{"params":{"duration":120}}}]`)}, nil},
		{serviceapi.Event{Type: "incoming_call", ValueAfter: json.RawMessage(`{"note":{"params":{"duration":"120"}}}`)}, number(120)},
		{serviceapi.Event{Type: "incoming_call", ValueAfter: json.RawMessage(`[{"note":{"params":{"duration":-1}}}]`)}, nil},
		{serviceapi.Event{Type: "incoming_call_extra", CallFacts: &serviceapi.CallFacts{Duration: &zero}}, nil},
	} {
		got := ratingEventFromAPI(c.event)
		if (got.Duration == nil) != (c.want == nil) || got.Duration != nil && *got.Duration != *c.want {
			t.Fatalf("call=%+v got%v want%v", c.event, got.Duration, c.want)
		}
	}
}
func number(v float64) *float64 { return &v }

type ratingEventSource struct {
	cards     map[string]serviceapi.Event
	cardReads int
	serviceapi.CRMEvents
	pages   []serviceapi.QueryResult
	queries []serviceapi.Query
	repeat  bool
}

func (e *ratingEventSource) Query(_ context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	e.queries = append(e.queries, q)
	if e.repeat {
		return serviceapi.QueryResult{ReadVersion: serviceapi.PresentationReadVersion, NextCursor: "same"}, nil
	}
	i := len(e.queries) - 1
	if len(e.pages) == 1 {
		return e.pages[0], nil
	}
	return e.pages[i], nil
}
func ratingService(t *testing.T, events *ratingEventSource) (*Service, serviceapi.ActivityRatingRequest) {
	t.Helper()
	id := uuid.New()
	store := &memoryPanels{panels: map[uuid.UUID]serviceapi.ManagedPanel{id: {ID: id, EmployeeIDs: []int64{7, 8}, Enabled: true, ViewKeyVersion: 1, Timezone: "Europe/Moscow", Options: serviceapi.PanelOptions{PipelineIDs: []int64{10}}}}}
	policy := &fakePolicy{kind: serviceapi.PrincipalKindViewer, panelID: id, viewKeyVersion: 1}
	s := New(store, policy, events, &panelGateway{users: []serviceapi.User{{ID: 7}, {ID: 8}}})
	loc, _ := time.LoadLocation("Europe/Moscow")
	day, _ := time.ParseInLocation("2006-01-02", "2026-10-26", loc)
	return s, serviceapi.ActivityRatingRequest{Auth: serviceapi.Auth{Token: "verified"}, Day: "2026-10-26", From: day.Unix() + 9*3600, To: day.Unix() + 18*3600, StartMinutes: 540, EndMinutes: 1080, Surface: "dashboard"}
}
func TestActivityRatingsScopedScanAndProjection(t *testing.T) {
	zero := int64(0)
	duration := int64(121)
	src := &ratingEventSource{}
	s, req := ratingService(t, src)
	at := req.From + 60*60
	src.pages = []serviceapi.QueryResult{{ReadVersion: 3, Events: []serviceapi.Event{{ID: "1", CreatedAt: at, CreatedBy: 7, Type: "outgoing_call", CallFacts: &serviceapi.CallFacts{Duration: &duration}, ValueAfter: json.RawMessage(`[{"lead_status":{"pipeline_id":10}}]`)}}, NextCursor: "next"}, {ReadVersion: 3, Events: []serviceapi.Event{{ID: "2", CreatedAt: at + 10*60, CreatedBy: 7, Type: "incoming_call", CallFacts: &serviceapi.CallFacts{Duration: &zero}}, {ID: "3", CreatedAt: at + 20*60, CreatedBy: 7, Type: "incoming_call", CallFacts: &serviceapi.CallFacts{}}, {ID: "4", CreatedAt: at + 30*60, CreatedBy: 7, Type: "task_completed", ValueAfter: json.RawMessage(`[{"lead_status":{"pipeline_id":11}}]`)}}}}
	got, err := s.ViewActivityRatings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.FormulaVersion != serviceapi.DashboardActivityFormula || len(got.Employees) != 2 || got.Employees[0].Percent != 1.5 || got.Employees[1].Percent != 0 {
		t.Fatalf("dashboard=%+v", got)
	}
	if len(src.queries) != 2 || src.queries[0].Limit != 100 || !reflect.DeepEqual(src.queries[0].UserIDs, []int64{7, 8}) || src.queries[0].Buckets != serviceapi.BucketAuto || src.queries[1].Cursor != "next" {
		t.Fatalf("queries=%+v", src.queries)
	}
	src.queries = nil
	req.Surface = "profile"
	req.EmployeeIDs = []int64{7}
	got, err = s.ViewActivityRatings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Profile includes the event in pipeline11, and zero/unknown calls both
	// retain the old60second fallback, while dashboard distinguishes duration.
	if got.FormulaVersion != serviceapi.ProfileActivityFormula || len(got.Employees[0].MinutePercentages) != 1441 || got.Employees[0].Percent != 1.9 {
		t.Fatalf("profile=%+v", got.Employees)
	}
	src.queries = nil
	req.EmployeeIDs = []int64{99}
	if _, err := s.ViewActivityRatings(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.NotFound || len(src.queries) != 0 {
		t.Fatalf("foreign employee reached event history: %v", err)
	}
}
func TestActivityRatingsPageCapAndRepeatedCursor(t *testing.T) {
	src := &ratingEventSource{pages: []serviceapi.QueryResult{{ReadVersion: 3, NextCursor: "unused"}}}
	s, req := ratingService(t, src)
	// A source returning a fresh cursor on every read reaches exactly the
	// existing UI60page cap, and must declare the resulting score partial.
	src.pages = nil
	for i := 0; i < 60; i++ {
		src.pages = append(src.pages, serviceapi.QueryResult{ReadVersion: 3, NextCursor: strconv.Itoa(i + 1)})
	}
	got, err := s.ViewActivityRatings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.queries) != 60 || !got.Partial {
		t.Fatalf("cap reads=%d partial=%v", len(src.queries), got.Partial)
	}
	src.queries = nil
	src.repeat = true
	if _, err = s.ViewActivityRatings(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.Unavailable || len(src.queries) != 2 {
		t.Fatalf("repeated cursor=%v", err)
	}
}

func (e *ratingEventSource) GetEvent(_ context.Context, r serviceapi.EventRequest) (serviceapi.Event, error) {
	e.cardReads++
	return e.cards[r.EventID], nil
}
func TestActivityRatingLegacyCardKeepsObservationAndBound(t *testing.T) {
	src := &ratingEventSource{cards: map[string]serviceapi.Event{}}
	s, req := ratingService(t, src)
	duration := int64(600)
	original := serviceapi.Event{ID: "call", CreatedAt: req.From + 3600, CreatedBy: 7, Type: "incoming_call"}
	// Only duration changes on fallback, never the observation timestamp/type.
	src.cards["call"] = serviceapi.Event{ID: "call", CreatedAt: 0, CreatedBy: 7, Type: "incoming_call", CallFacts: &serviceapi.CallFacts{Duration: &duration}}
	src.pages = []serviceapi.QueryResult{{ReadVersion: 3, Events: []serviceapi.Event{original}}}
	got, err := s.ViewActivityRatings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if src.cardReads != 1 || got.Employees[0].Percent != 1.9 {
		t.Fatalf("legacy observation rating=%v reads=%d", got.Employees, src.cardReads)
	}
	src.queries = nil
	src.cardReads = 0
	src.pages = []serviceapi.QueryResult{{ReadVersion: 3, NextCursor: "p2"}, {ReadVersion: 3}}
	for i := 0; i < 151; i++ {
		e := original
		e.ID = strconv.Itoa(i)
		e.ValueAfter = json.RawMessage(`[{"pipeline_id":11}]`)
		if i == 150 {
			e.ValueAfter = json.RawMessage(`[{"pipeline_id":10}]`)
		}
		src.cards[e.ID] = serviceapi.Event{Type: "incoming_call", CallFacts: &serviceapi.CallFacts{Duration: &duration}}
		page := 0
		if i >= 100 {
			page = 1
		}
		src.pages[page].Events = append(src.pages[page].Events, e)
	}
	got, err = s.ViewActivityRatings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if src.cardReads != 150 || got.Employees[0].Percent != 0.4 {
		t.Fatalf("legacy card cap/filter order rating=%v reads=%d", got.Employees, src.cardReads)
	}
}
