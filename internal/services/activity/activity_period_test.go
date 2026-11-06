package activity

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// periodEventSource serves events by inclusive [From, To] and author, with
// offset cursors, like CRM Events.
type periodEventSource struct {
	serviceapi.CRMEvents
	mu        sync.Mutex
	events    []serviceapi.Event
	status    serviceapi.SyncStatus
	queries   []serviceapi.Query
	endless   int64 // From of a day whose pages never end
	blocked   int64 // From of a day whose query waits for cancellation
	pageLimit int
	repeat    bool
	cards     map[string]serviceapi.Event
	reads     []string
}

func (e *periodEventSource) GetEvent(_ context.Context, r serviceapi.EventRequest) (serviceapi.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads = append(e.reads, r.EventID)
	return e.cards[r.EventID], nil
}

func (e *periodEventSource) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	if e.blocked != 0 && q.From == e.blocked {
		<-ctx.Done()
		return serviceapi.QueryResult{}, ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, q)
	if err := serviceapi.ValidateQuery(q); err != nil {
		return serviceapi.QueryResult{}, err
	}
	if e.pageLimit > 0 && q.Limit > e.pageLimit {
		return serviceapi.QueryResult{}, serviceapi.ValidateResponseSize(make([]byte, serviceapi.MaxResponseBytes))
	}
	out := serviceapi.QueryResult{ReadVersion: serviceapi.PresentationReadVersion, Status: e.status, Events: []serviceapi.Event{}}
	if e.endless != 0 && q.From == e.endless {
		out.NextCursor = "c" + strconv.Itoa(len(e.queries))
		if e.repeat {
			out.NextCursor = "same"
		}
		return out, nil
	}
	offset, _ := strconv.Atoi(q.Cursor)
	var matched []serviceapi.Event
	for _, event := range e.events {
		if event.CreatedAt >= q.From && event.CreatedAt <= q.To && slices.Contains(q.UserIDs, event.CreatedBy) {
			matched = append(matched, event)
		}
	}
	end := min(len(matched), offset+q.Limit)
	out.Events = append(out.Events, matched[offset:end]...)
	if end < len(matched) {
		out.NextCursor = strconv.Itoa(end)
	}
	return out, nil
}

var periodNow = time.Date(2026, 11, 5, 14, 30, 20, 0, time.FixedZone("MSK", 3*3600)) // Thursday

func periodService(t *testing.T, src *periodEventSource, now time.Time) *Service {
	t.Helper()
	id := uuid.New()
	store := &memoryPanels{panels: map[uuid.UUID]serviceapi.ManagedPanel{id: {ID: id, EmployeeIDs: []int64{7, 8}, Enabled: true, ViewKeyVersion: 1, Timezone: "Europe/Moscow", Options: serviceapi.PanelOptions{PipelineIDs: []int64{10}}}}}
	policy := &fakePolicy{kind: serviceapi.PrincipalKindViewer, panelID: id, viewKeyVersion: 1}
	s := New(store, policy, src, &panelGateway{users: []serviceapi.User{{ID: 7}, {ID: 8}}})
	s.now = func() time.Time { return now }
	return s
}

func msk(t *testing.T, day string, hour, minute int) int64 {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Moscow")
	d, err := time.ParseInLocation(time.DateOnly, day, loc)
	if err != nil {
		t.Fatal(err)
	}
	return d.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute).Unix()
}

func periodReq(kind, anchor string) serviceapi.ActivityPeriodRequest {
	return serviceapi.ActivityPeriodRequest{Auth: serviceapi.Auth{Token: "verified"}, Kind: kind, Anchor: anchor, StartMinutes: 540, EndMinutes: 1080}
}

func ratio(t *testing.T, got *float64, active, elapsed int) {
	t.Helper()
	if got == nil || math.Abs(*got-float64(active)/float64(elapsed)) > 1e-12 {
		t.Fatalf("rating %v, want %d/%d", got, active, elapsed)
	}
}

func TestActivityPeriodWeekCutAndPrevious(t *testing.T) {
	ten := int64(600)
	src := &periodEventSource{status: serviceapi.SyncStatus{HistoryFrom: periodNow.Unix() - 40*86400, VerifiedFrom: periodNow.Unix() - 40*86400, VerifiedThrough: periodNow.Unix() - 60}}
	src.events = []serviceapi.Event{
		{ID: "mon-call", CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 10, 0), Type: "outgoing_call", EntityType: "lead", CallFacts: &serviceapi.CallFacts{Duration: &ten}, ValueAfter: json.RawMessage(`[{"lead_status":{"pipeline_id":10}}]`)},
		{ID: "mon-foreign-pipeline", CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 12, 0), Type: "task_completed", ValueAfter: json.RawMessage(`[{"pipeline_id":11}]`)},
		{ID: "mon-evening", CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 21, 0), Type: "task_completed"},
		{ID: "today", CreatedBy: 7, CreatedAt: msk(t, "2026-11-05", 10, 0), Type: "task_completed", EntityType: "task"},
		{ID: "today-later", CreatedBy: 7, CreatedAt: msk(t, "2026-11-05", 14, 40), Type: "task_completed"},
		{ID: "prev-thu", CreatedBy: 7, CreatedAt: msk(t, "2026-10-29", 10, 0), Type: "lead_status_changed", EntityType: "lead"},
		{ID: "prev-thu-late", CreatedBy: 7, CreatedAt: msk(t, "2026-10-29", 16, 0), Type: "lead_status_changed", EntityType: "lead"},
		{ID: "prev-sat", CreatedBy: 7, CreatedAt: msk(t, "2026-10-31", 12, 0), Type: "entity_direct_message", EntityType: "contact"},
	}
	s := periodService(t, src, periodNow)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-07"))
	if err != nil {
		t.Fatal(err)
	}
	if got.FormulaVersion != "period-v1" || got.Kind != "week" || got.Timezone != "Europe/Moscow" || got.CutoffUnix != periodNow.Unix() || got.HistoryFrom != "2026-10-04" || got.RefreshAfter != nil {
		t.Fatalf("envelope %+v", got)
	}
	if !slices.Equal(got.Hours, []int{9, 10, 11, 12, 13, 14, 15, 16, 17}) || got.Current.Start != "2026-11-02" || got.Current.End != "2026-11-08" || len(got.Current.Days) != 7 {
		t.Fatalf("current %+v hours %v", got.Current, got.Hours)
	}
	if got.Previous.Start != "2026-10-26" || got.Previous.End != "2026-11-01" || !got.Previous.Available || got.Previous.Cut == nil || *got.Previous.Cut != (serviceapi.ActivityPeriodCut{DayIndex: 3, Minute: 870}) {
		t.Fatalf("previous %+v cut %+v", got.Previous, got.Previous.Cut)
	}
	if len(got.Employees) != 2 || got.Employees[0].EmployeeID != 7 || got.Employees[1].EmployeeID != 8 {
		t.Fatalf("employees %+v", got.Employees)
	}
	cur := got.Employees[0].Current
	mon, thu, fri := cur.Days[0], cur.Days[3], cur.Days[4]
	if mon.State != "ready" || !mon.Counted || mon.Active != 10 || mon.Call != 10 || mon.Elapsed != 540 || !slices.Equal(mon.KPI, []int{1, 0, 0, 0, 0}) || mon.Events != 1 || *mon.First != 600 || *mon.Last != 610 || mon.Coverage != "verified" || mon.Partial || len(mon.Hours) != 9 || mon.Hours[1] != [3]int{10, 10, 60} {
		t.Fatalf("monday %+v", *mon.ActivityDayValues)
	}
	if thu.State != "today" || !thu.Counted || thu.Elapsed != 330 || thu.Active != 2 || !slices.Equal(thu.KPI, []int{0, 0, 0, 1, 0}) || thu.Coverage != "partial" {
		t.Fatalf("today %+v", *thu.ActivityDayValues)
	}
	if fri.State != "future" || fri.Counted || fri.ActivityDayValues != nil {
		t.Fatalf("future %+v", fri)
	}
	ratio(t, cur.Rating, 12, 3*540+330)
	prev := got.Employees[0].Previous
	if prev == nil || len(prev.Days) != 7 || prev.CutDay == nil {
		t.Fatalf("previous %+v", prev)
	}
	if prev.Days[3].Active != 4 || prev.Days[3].Elapsed != 540 || prev.CutDay.Day != "2026-10-29" || prev.CutDay.Active != 2 || prev.CutDay.Elapsed != 330 || prev.CutDay.KPI[4] != 1 {
		t.Fatalf("cut day %+v full %+v", *prev.CutDay.ActivityDayValues, *prev.Days[3].ActivityDayValues)
	}
	if !prev.Days[5].Counted || prev.Days[5].Active != 2 || prev.Days[6].Counted {
		t.Fatalf("weekend rule sat %+v sun %+v", prev.Days[5], prev.Days[6])
	}
	ratio(t, prev.RatingFull, 6, 6*540)
	ratio(t, prev.Rating, 2, 3*540+330)
	empty := got.Employees[1]
	ratio(t, empty.Current.Rating, 0, 3*540+330)
	if empty.Previous == nil || empty.Previous.Days[6].Counted {
		t.Fatalf("employee without events %+v", empty.Previous)
	}
	// One bounded query batch per day from local midnight to an hour past the
	// window end (today: the cutoff); no read of future days.
	days := map[int64]serviceapi.Query{}
	for _, q := range src.queries {
		if q.Limit != 100 || !slices.Equal(q.UserIDs, []int64{7, 8}) || q.Cursor != "" {
			t.Fatalf("query %+v", q)
		}
		days[q.From] = q
	}
	if len(src.queries) != 11 || len(days) != 11 {
		t.Fatalf("queries %d", len(src.queries))
	}
	if q := days[msk(t, "2026-11-02", 0, 0)]; q.To != msk(t, "2026-11-02", 19, 0)-1 {
		t.Fatalf("monday read %+v", q)
	}
	if q := days[msk(t, "2026-11-05", 0, 0)]; q.To != periodNow.Unix() {
		t.Fatalf("today read %+v", q)
	}
}

func TestActivityPeriodMonthCutHistoryAndStates(t *testing.T) {
	old := serviceapi.SyncStatus{HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: 1}
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	src := &periodEventSource{status: old}
	s := periodService(t, src, monday)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("month", "2026-11-20"))
	if err != nil {
		t.Fatal(err)
	}
	// Day of month - 1; the 31-day October is wholly inside 33 days of history.
	if len(got.Current.Days) != 30 || got.Current.Start != "2026-11-01" || got.Previous.Start != "2026-10-01" || len(got.Previous.Days) != 31 || !got.Previous.Available || *got.Previous.Cut != (serviceapi.ActivityPeriodCut{DayIndex: 1, Minute: 720}) || got.HistoryFrom != "2026-10-01" {
		t.Fatalf("month %+v cut %+v history %s", got.Previous, got.Previous.Cut, got.HistoryFrom)
	}
	if prev := got.Employees[0].Previous; prev == nil || len(prev.Days) != 31 || prev.CutDay.Day != "2026-10-02" || prev.CutDay.Elapsed != 180 {
		t.Fatalf("month previous %+v", prev)
	}
	if d := got.Employees[0].Current.Days; d[0].Counted || d[0].State != "ready" || d[1].State != "today" || d[2].State != "future" {
		t.Fatalf("month states %+v %+v %+v", d[0], d[1], d[2])
	}
	// A later first whole day of CRM history removes the comparison.
	src.status = serviceapi.SyncStatus{HistoryFrom: msk(t, "2026-10-15", 10, 0)}
	got, err = s.ViewActivityPeriod(context.Background(), periodReq("month", "2026-11-20"))
	if err != nil {
		t.Fatal(err)
	}
	if got.HistoryFrom != "2026-10-16" || got.Previous.Available || got.Employees[0].Previous != nil || got.Previous.Cut == nil {
		t.Fatalf("late history %+v", got.Previous)
	}
	src.status = serviceapi.SyncStatus{HistoryFrom: msk(t, "2026-10-15", 0, 0)}
	if got, _ = s.ViewActivityPeriod(context.Background(), periodReq("month", "2026-10-15")); got.HistoryFrom != "2026-10-15" || got.Employees[0].Current.Days[13].State != "unavailable" || got.Employees[0].Current.Days[14].State != "ready" || got.Previous.Cut != nil {
		t.Fatalf("history at midnight / past period %s %+v", got.HistoryFrom, got.Employees[0].Current.Days[13:15])
	}
	// Past period: no cut, previous wholly outside history.
	src.status = old
	src.queries = nil
	got, err = s.ViewActivityPeriod(context.Background(), periodReq("month", "2026-10-31"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Previous.Cut != nil || got.Previous.Available || got.Previous.Start != "2026-09-01" || got.Employees[0].Current.Rating == nil || len(src.queries) != 31 {
		t.Fatalf("past month %+v queries %d", got.Previous, len(src.queries))
	}
	// Future period: nothing read except one probe for the history bound.
	src.queries = nil
	got, err = s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-12-01"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Employees[0].Current.Rating != nil || got.Previous.Available || got.Employees[0].Current.Days[0].State != "future" || len(src.queries) != 1 || src.queries[0].Limit != 1 {
		t.Fatalf("future %+v queries %+v", got.Employees[0].Current, src.queries)
	}
	// A previous month shorter than today's index has no same point.
	march := time.Date(2026, 3, 31, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	got, err = periodService(t, &periodEventSource{status: old}, march).ViewActivityPeriod(context.Background(), periodReq("month", "2026-03-01"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Previous.Cut != nil || got.Previous.Available || len(got.Previous.Days) != 28 {
		t.Fatalf("short previous month %+v", got.Previous)
	}
}

func TestActivityPeriodNullRatingAndWeekendOnly(t *testing.T) {
	early := time.Date(2026, 11, 2, 8, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	s := periodService(t, &periodEventSource{}, early)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-02"))
	if err != nil {
		t.Fatal(err)
	}
	today := got.Employees[0].Current.Days[0]
	if got.Employees[0].Current.Rating != nil || today.State != "today" || today.Elapsed != 0 || !today.Counted || today.First != nil {
		t.Fatalf("Σelapsed=0 must be null: %+v", got.Employees[0].Current)
	}
	// Explicit cutoff decides today; Sunday without activity is not counted.
	sunday := msk(t, "2026-11-01", 20, 0)
	req := periodReq("week", "2026-11-01")
	req.CutoffUnix = &sunday
	req.EmployeeIDs = []int64{8}
	got, err = periodService(t, &periodEventSource{}, periodNow).ViewActivityPeriod(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.CutoffUnix != sunday || len(got.Employees) != 1 || got.Employees[0].EmployeeID != 8 || got.Employees[0].Current.Days[6].State != "today" || got.Employees[0].Current.Days[6].Counted || got.Previous.Cut.DayIndex != 6 {
		t.Fatalf("sunday cutoff %+v", got)
	}
	ratio(t, got.Employees[0].Current.Rating, 0, 5*540)
}

func TestActivityPeriodPartialBoundsAndErrors(t *testing.T) {
	src := &periodEventSource{endless: msk(t, "2026-11-03", 0, 0)}
	s := periodService(t, src, periodNow)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	days := got.Employees[0].Current.Days
	if !days[1].Partial || days[0].Partial || days[2].Partial {
		t.Fatalf("page bound must mark only that day partial: %+v %+v", days[0], days[1])
	}
	pages := 0
	for _, q := range src.queries {
		if q.From == src.endless {
			pages++
		}
	}
	if pages != serviceapi.ActivityPeriodDayMaxPages {
		t.Fatalf("pages %d", pages)
	}
	src.repeat = true
	if _, err = s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05")); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("repeated cursor %v", err)
	}
	// An exhausted scan budget degrades to partial days, not an error.
	src = &periodEventSource{events: []serviceapi.Event{{CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 10, 0), Type: "task_added"}}}
	s = periodService(t, src, periodNow)
	s.periodScanBudget = time.Nanosecond
	got, err = s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	if mon := got.Employees[0].Current.Days[0]; !mon.Partial || mon.Active != 0 || len(src.queries) != 1 {
		t.Fatalf("budget %+v queries %d", mon, len(src.queries))
	}
	src.queries = nil
	for _, bad := range []serviceapi.ActivityPeriodRequest{
		{Auth: serviceapi.Auth{Token: "verified"}, Kind: "day", Anchor: "2026-11-05", StartMinutes: 540, EndMinutes: 1080},
		{Auth: serviceapi.Auth{Token: "verified"}, Kind: "week", Anchor: "2026-11-31", StartMinutes: 540, EndMinutes: 1080},
		{Auth: serviceapi.Auth{Token: "verified"}, Kind: "week", Anchor: "2026-11-05", StartMinutes: 600, EndMinutes: 600},
		{Auth: serviceapi.Auth{Token: "verified"}, Kind: "week", Anchor: "2026-11-05", StartMinutes: 1440, EndMinutes: 1441},
		{Auth: serviceapi.Auth{Token: "verified"}, Kind: "week", Anchor: "2026-11-05", StartMinutes: 0, EndMinutes: 1440, EmployeeIDs: []int64{7, 7}},
	} {
		if _, err := s.ViewActivityPeriod(context.Background(), bad); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	req := periodReq("week", "2026-11-05")
	req.EmployeeIDs = []int64{7, 99}
	if _, err := s.ViewActivityPeriod(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.NotFound || len(src.queries) != 0 {
		t.Fatalf("foreign employee reached events: %v", err)
	}
}

func TestActivityPeriodDaysSegments(t *testing.T) {
	two := int64(120)
	src := &periodEventSource{status: serviceapi.SyncStatus{HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: periodNow.Unix() - 60}}
	src.events = []serviceapi.Event{
		{CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 14, 22), Type: "outgoing_call", CallFacts: &serviceapi.CallFacts{Duration: &two}},
		{CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 14, 25), Type: "lead_status_changed", EntityType: "lead"},
		{CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 14, 27), Type: "task_added", ValueAfter: json.RawMessage(`[{"pipeline_id":11}]`)},
		{CreatedBy: 8, CreatedAt: msk(t, "2026-11-02", 14, 29), Type: "task_added"},
		{CreatedBy: 7, CreatedAt: msk(t, "2026-11-05", 14, 21), Type: "task_added"},
	}
	s := periodService(t, src, periodNow)
	req := serviceapi.ActivityPeriodDaysRequest{Auth: serviceapi.Auth{Token: "verified"}, EmployeeID: 7, FromDay: "2026-11-02", ToDay: "2026-11-08", StartMinutes: 860, EndMinutes: 880}
	got, err := s.ViewActivityPeriodDays(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.FormulaVersion != "period-v1" || got.SegmentMinutes != 2 || got.EmployeeID != 7 || got.CutoffUnix != periodNow.Unix() || len(got.Days) != 7 {
		t.Fatalf("envelope %+v", got)
	}
	mon, thu, fri := got.Days[0], got.Days[3], got.Days[4]
	if mon.State != "ready" || mon.Kinds != "0210000000" || mon.Zones != "-CD-------" || *mon.First != 862 || *mon.Last != 866 || mon.Coverage != "verified" {
		t.Fatalf("monday %+v", *mon.ActivityPeriodDaySegments)
	}
	if thu.State != "today" || thu.Kinds != "10000....." || thu.Zones != "T----....." {
		t.Fatalf("today %+v", *thu.ActivityPeriodDaySegments)
	}
	if fri.State != "future" || fri.ActivityPeriodDaySegments != nil {
		t.Fatalf("future %+v", fri)
	}
	for _, q := range src.queries {
		if !slices.Equal(q.UserIDs, []int64{7}) {
			t.Fatalf("query %+v", q)
		}
	}
	src.queries = nil
	for _, bad := range []serviceapi.ActivityPeriodDaysRequest{
		{Auth: req.Auth, EmployeeID: 7, FromDay: "2026-10-01", ToDay: "2026-11-01", StartMinutes: 540, EndMinutes: 1080},
		{Auth: req.Auth, EmployeeID: 7, FromDay: "2026-11-02", ToDay: "2026-11-01", StartMinutes: 540, EndMinutes: 1080},
		{Auth: req.Auth, EmployeeID: 0, FromDay: "2026-11-01", ToDay: "2026-11-01", StartMinutes: 540, EndMinutes: 1080},
		{Auth: req.Auth, EmployeeID: 7, FromDay: "2026-11-01", ToDay: "2026-11-01", StartMinutes: 540, EndMinutes: 1441},
	} {
		if _, err := s.ViewActivityPeriodDays(context.Background(), bad); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	if _, err := s.ViewActivityPeriodDays(context.Background(), serviceapi.ActivityPeriodDaysRequest{Auth: req.Auth, EmployeeID: 7, FromDay: "2026-10-03", ToDay: "2026-11-02", StartMinutes: 540, EndMinutes: 1080}); err != nil {
		t.Fatalf("31 days rejected: %v", err)
	}
	src.queries = nil
	req.EmployeeID = 99
	if _, err := s.ViewActivityPeriodDays(context.Background(), req); serviceapi.ErrorCode(err) != serviceapi.NotFound || len(src.queries) != 0 {
		t.Fatalf("foreign employee %v", err)
	}
}

func TestActivityPeriodJSONShape(t *testing.T) {
	first := 548
	stat := serviceapi.ActivityDayStat{Day: "2026-11-02", State: "ready", Counted: true, ActivityDayValues: &serviceapi.ActivityDayValues{Coverage: "verified", KPI: []int{1, 2, 3, 4, 5}, Events: 15, Active: 2, Elapsed: 540, First: &first, Hours: [][3]int{{2, 0, 60}}}}
	raw, _ := json.Marshal([]serviceapi.ActivityDayStat{stat, {Day: "2026-11-03", State: "future"}})
	want := `[{"day":"2026-11-02","state":"ready","counted":true,"coverage":"verified","partial":false,"kpi":[1,2,3,4,5],"events":15,"active":2,"call":0,"elapsed":540,"idle_inside":0,"first":548,"last":null,"hours":[[2,0,60]]},{"day":"2026-11-03","state":"future","counted":false}]`
	if string(raw) != want {
		t.Fatalf("day stat json\n%s\nwant\n%s", raw, want)
	}
	raw, _ = json.Marshal(serviceapi.ActivityPreviousPeriod{ActivityPeriodRange: serviceapi.ActivityPeriodRange{Start: "a", End: "b", Days: []string{"a"}}})
	if string(raw) != `{"start":"a","end":"b","days":["a"],"available":false,"cut":null}` {
		t.Fatalf("previous json %s", raw)
	}
}

// The client asks for 25 employees per request; a full-day month with a
// complete previous month must stay far below the response limit.
func TestActivityPeriodResponseSizeForClientBatch(t *testing.T) {
	first, last := 1, 1439
	hours := make([][3]int, 24)
	for i := range hours {
		hours[i] = [3]int{60, 60, 60}
	}
	day := serviceapi.ActivityDayStat{Day: "2026-10-31", State: "ready", Counted: true, ActivityDayValues: &serviceapi.ActivityDayValues{Coverage: "verified", Partial: true, KPI: []int{999, 999, 999, 999, 999}, Events: 4995, Active: 1440, Call: 1440, Elapsed: 1440, IdleInside: 1438, First: &first, Last: &last, Hours: hours}}
	days := slices.Repeat([]serviceapi.ActivityDayStat{day}, 31)
	rating := 0.123456789
	size := func(employees int) int {
		out := serviceapi.ActivityPeriod{Employees: make([]serviceapi.EmployeeActivityPeriod, employees)}
		for i := range out.Employees {
			out.Employees[i] = serviceapi.EmployeeActivityPeriod{EmployeeID: 9007199254740991, Current: serviceapi.ActivityPeriodCurrent{Rating: &rating, Days: days}, Previous: &serviceapi.ActivityPeriodPrevious{Rating: &rating, RatingFull: &rating, Days: days, CutDay: &day}}
		}
		raw, _ := json.Marshal(out)
		return len(raw)
	}
	if got := size(25); got > serviceapi.MaxResponseBytes/2 {
		t.Fatalf("25 employees: %d bytes", got)
	}
	if got := size(100); got > serviceapi.MaxResponseBytes {
		t.Logf("100 employees with full-day windows exceed the limit (%d bytes) and fail as resource_exhausted", got)
	}
}

// Calls without call facts take their duration from the event card, as the
// daily rating does: at most 150 per request, newest day first.
func TestActivityPeriodCallCardFallback(t *testing.T) {
	ten := int64(600)
	src := &periodEventSource{cards: map[string]serviceapi.Event{}}
	src.events = append(src.events, serviceapi.Event{ID: "mon", CreatedBy: 7, CreatedAt: msk(t, "2026-11-02", 10, 0), Type: "incoming_call"})
	src.cards["mon"] = serviceapi.Event{Type: "incoming_call", CallFacts: &serviceapi.CallFacts{Duration: &ten}}
	s := periodService(t, src, periodNow)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	if mon := got.Employees[0].Current.Days[0]; mon.Active != 10 || mon.Call != 10 || len(src.reads) != 1 {
		t.Fatalf("card duration not applied %+v reads %v", *mon.ActivityDayValues, src.reads)
	}
	src.reads = nil
	for i := 0; i < 150; i++ {
		id := "today-" + strconv.Itoa(i)
		src.events = append(src.events, serviceapi.Event{ID: id, CreatedBy: 8, CreatedAt: msk(t, "2026-11-05", 9, i%300), Type: "outgoing_call"})
		src.cards[id] = serviceapi.Event{Type: "outgoing_call", CallFacts: &serviceapi.CallFacts{Duration: &ten}}
	}
	got, err = s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	if mon := got.Employees[0].Current.Days[0]; len(src.reads) != 150 || slices.Contains(src.reads, "mon") || mon.Active != 2 || got.Employees[1].Current.Days[3].Call == 0 {
		t.Fatalf("card bound reads %d monday %+v", len(src.reads), *mon.ActivityDayValues)
	}
}

func TestActivityPeriodInFlightBudgetMarksDayPartial(t *testing.T) {
	src := &periodEventSource{blocked: msk(t, "2026-11-02", 0, 0)}
	s := periodService(t, src, periodNow)
	s.periodScanBudget = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := s.ViewActivityPeriod(ctx, periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	if mon := got.Employees[0].Current.Days[0]; !mon.Partial {
		t.Fatalf("blocked day must be partial: %+v", mon)
	}
}

func TestActivityPeriodCardLimitMarksOmittedDayPartial(t *testing.T) {
	duration := int64(30)
	src := &periodEventSource{cards: make(map[string]serviceapi.Event)}
	for i := range serviceapi.ActivityPeriodMaxCards + 1 {
		id := strconv.Itoa(i)
		at := periodNow.Add(-time.Hour).Unix()
		if i == serviceapi.ActivityPeriodMaxCards {
			at = msk(t, "2026-11-02", 10, 0)
		}
		src.events = append(src.events, serviceapi.Event{ID: id, CreatedAt: at, CreatedBy: 7, Type: "outgoing_call"})
		src.cards[id] = serviceapi.Event{ID: id, Type: "outgoing_call", CallFacts: &serviceapi.CallFacts{Duration: &duration}}
	}
	s := periodService(t, src, periodNow)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	days := got.Employees[0].Current.Days
	if !days[0].Partial || days[3].Partial || len(src.reads) != serviceapi.ActivityPeriodMaxCards {
		t.Fatalf("omitted day partial=%v, resolved day partial=%v, reads=%d", days[0].Partial, days[3].Partial, len(src.reads))
	}
}

func TestActivityPeriodRetriesOversizedPages(t *testing.T) {
	src := &periodEventSource{pageLimit: 50}
	at := msk(t, "2026-11-02", 10, 0)
	for i := range 51 {
		src.events = append(src.events, serviceapi.Event{ID: strconv.Itoa(i), CreatedAt: at, CreatedBy: 7, Type: "task_completed"})
	}
	s := periodService(t, src, periodNow)
	got, err := s.ViewActivityPeriod(context.Background(), periodReq("week", "2026-11-05"))
	if err != nil {
		t.Fatal(err)
	}
	mon := got.Employees[0].Current.Days[0]
	if mon.Partial || mon.Events != 51 {
		t.Fatalf("Monday events=%d, partial=%v", mon.Events, mon.Partial)
	}
}
