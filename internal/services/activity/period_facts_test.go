package activity

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func periodTestDay(t *testing.T, day string) (*time.Location, time.Time) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.ParseInLocation(time.DateOnly, day, loc)
	if err != nil {
		t.Fatal(err)
	}
	return loc, start
}

func clock(day time.Time, hour, minute, second int) int64 {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute + time.Duration(second)*time.Second).Unix()
}

func pevent(at int64, eventType string, duration *float64) periodEvent {
	return periodEvent{ratingEvent: ratingEvent{At: at, Type: eventType, Duration: duration}}
}

func ratingEvents(events []periodEvent) []ratingEvent {
	out := make([]ratingEvent, 0, len(events))
	for _, e := range events {
		out = append(out, e.ratingEvent)
	}
	return out
}

func cutMinute(cutoff *int64, loc *time.Location) *int {
	if cutoff == nil {
		return nil
	}
	at := time.Unix(*cutoff, 0).In(loc)
	m := at.Hour()*60 + at.Minute()
	return &m
}

// assertDashboardParity proves roundedRating(active, elapsed) — that is
// Math.round((active/elapsed)*100*10)/10 in this order — equals the output of
// dashboardRating itself, not a re-derived formula.
func assertDashboardParity(t *testing.T, events []periodEvent, day string, loc *time.Location, w periodWindow, cutoff *int64) dayStat {
	t.Helper()
	stat := DayStatFrom(BuildDayFacts(events, day, loc), w, cutMinute(cutoff, loc), nil)
	want := dashboardRating(ratingEvents(events), day, loc, w.Start, w.End, cutoff)
	if stat.Elapsed == 0 {
		if want != 0 || stat.Active != 0 {
			t.Fatalf("empty range active %d dashboard %v", stat.Active, want)
		}
		return stat
	}
	if got := roundedRating(float64(stat.Active), float64(stat.Elapsed)); got != want {
		t.Fatalf("window %+v cutoff %v: active %d/%d = %v, dashboard %v", w, cutoff, stat.Active, stat.Elapsed, got, want)
	}
	return stat
}

func TestPeriodDayStatDashboardParityCases(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	cut := func(h, m, s int) *int64 { v := clock(start, h, m, s); return &v }
	calls := []periodEvent{
		pevent(clock(start, 10, 0, 30), "outgoing_call", number(121)),
		pevent(clock(start, 10, 20, 0), "incoming_call", nil),
		pevent(clock(start, 10, 40, 0), "incoming_call", number(0)),
		pevent(clock(start, 11, 0, 0), "outgoing_call", number(3600)),
		pevent(clock(start, 12, 15, 59), "task_completed", nil),
	}
	overlapping := []periodEvent{
		pevent(clock(start, 8, 50, 0), "outgoing_call", number(1260)),
		pevent(clock(start, 9, 5, 0), "task_added", nil),
		pevent(clock(start, 9, 6, 0), "incoming_chat_message", nil),
		pevent(clock(start, 9, 6, 40), "lead_status_changed", nil),
		pevent(clock(start, 17, 59, 0), "outgoing_call", number(600)),
		pevent(clock(start, -1, 0, 0), "outgoing_call", number(7200)),
		pevent(clock(start, 24, 1, 0), "outgoing_call", number(60)),
	}
	late := []periodEvent{pevent(clock(start, 23, 58, 0), "outgoing_call", number(1e9))}
	for _, c := range []struct {
		name   string
		events []periodEvent
		window periodWindow
		cutoff *int64
	}{
		{"calls with and without duration", calls, periodWindow{540, 1080}, nil},
		{"overlapping and outside day", overlapping, periodWindow{540, 1080}, nil},
		{"cutoff mid hour", calls, periodWindow{540, 1080}, cut(10, 37, 45)},
		{"cutoff after window", overlapping, periodWindow{540, 1080}, cut(19, 10, 0)},
		{"cutoff in last window hour", overlapping, periodWindow{540, 1080}, cut(18, 30, 0)},
		{"cutoff before window", calls, periodWindow{540, 1080}, cut(8, 30, 0)},
		{"odd window short last segment", calls, periodWindow{541, 1079}, nil},
		{"odd window cut", overlapping, periodWindow{541, 1079}, cut(9, 7, 0)},
		{"empty day", nil, periodWindow{540, 1080}, nil},
		{"empty day cut", nil, periodWindow{0, 1440}, cut(23, 59, 59)},
		{"midnight overflow", late, periodWindow{0, 1440}, nil},
		{"one minute window", overlapping, periodWindow{545, 546}, nil},
	} {
		t.Run(c.name, func(t *testing.T) { assertDashboardParity(t, c.events, day, loc, c.window, c.cutoff) })
	}
	full := assertDashboardParity(t, late, day, loc, periodWindow{0, 1440}, nil)
	if full.Active != 2 || *full.First != 1438 || *full.Last != 1440 {
		t.Fatalf("occupancy must stop at midnight: %+v", full)
	}
}

func TestPeriodDayStatDashboardParityRandomized(t *testing.T) {
	const day = "2026-10-31"
	loc, start := periodTestDay(t, day)
	rng := rand.New(rand.NewSource(20261106))
	types := []string{"outgoing_call", "incoming_call", "task_completed", "task_added", "lead_status_changed", "entity_direct_message", "outgoing_mail", "contact_added", "incoming_chat_message"}
	for i := 0; i < 3000; i++ {
		events := make([]periodEvent, rng.Intn(40))
		for j := range events {
			var duration *float64
			switch rng.Intn(4) {
			case 1:
				duration = number(0)
			case 2, 3:
				duration = number(float64(rng.Intn(5400)))
			}
			events[j] = pevent(start.Unix()+int64(rng.Intn(90000))-1800, types[rng.Intn(len(types))], duration)
		}
		w := periodWindow{Start: rng.Intn(1440)}
		w.End = w.Start + 1 + rng.Intn(1440-w.Start)
		var cutoff *int64
		if rng.Intn(2) == 0 {
			v := start.Unix() + int64(rng.Intn(86400))
			cutoff = &v
		}
		assertDashboardParity(t, events, day, loc, w, cutoff)
	}
}

func TestPeriodZonesKindsAndKPI(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	ev := func(minute int, eventType, entityType string) periodEvent {
		e := pevent(clock(start, 10, minute, 0), eventType, nil)
		e.EntityType = entityType
		return e
	}
	events := []periodEvent{
		ev(0, "outgoing_call", "lead"), ev(0, "task_completed", "task"), // C beats T
		ev(2, "task_added", "lead"), ev(2, "lead_status_changed", "lead"), // T beats D
		ev(4, "entity_direct_message", "lead"), ev(5, "lead_added", "lead"), // I beats D
		ev(6, "lead_added", "leads"),
		ev(8, "custom_field_value_changed", "tasks"), // entity task
		ev(10, "outgoing_sms", "contact"),
		ev(12, "contact_added", "contact"),
		ev(16, "outgoing_mail", "contact"), ev(16, "incoming_mail", "contact"),
		ev(18, "incoming_chat_message", "contact"), ev(18, "outgoing_call", "contact"),
	}
	w := periodWindow{600, 620}
	stat := DayStatFrom(BuildDayFacts(events, day, loc), w, nil, nil)
	kinds, zones := stat.segments()
	if kinds != "2111111012" || zones != "CTIDTIO-OC" {
		t.Fatalf("kinds %q zones %q", kinds, zones)
	}
	if stat.KPI != [periodKPICount]int{2, 3, 1, 1, 1} || stat.Events != 8 {
		t.Fatalf("kpi %v events %d", stat.KPI, stat.Events)
	}
	if stat.Call != 4 || stat.Active != 18 || stat.IdleInside != 2 || *stat.First != 600 || *stat.Last != 620 {
		t.Fatalf("stat %+v", stat)
	}
	// Two events of one minute are two KPI entries; minutes outside the range
	// (before the window or after the cut) are not counted.
	events = append(events, ev(0, "outgoing_call", ""), ev(-1, "outgoing_call", ""), ev(19, "task_completed", ""))
	cut := 600 + 17
	stat = DayStatFrom(BuildDayFacts(events, day, loc), w, &cut, nil)
	if stat.KPI != [periodKPICount]int{2, 2, 1, 1, 1} || stat.Elapsed != 17 {
		t.Fatalf("cut kpi %v elapsed %d", stat.KPI, stat.Elapsed)
	}
	kinds, zones = stat.segments()
	if kinds != "211111101." || zones != "CTIDTIO-O." {
		t.Fatalf("cut kinds %q zones %q", kinds, zones)
	}
	if facts := BuildDayFacts(events, day, loc); facts.CallsWithoutDuration != 4 {
		t.Fatalf("calls without duration %d", facts.CallsWithoutDuration)
	}
}

func TestPeriodFirstLastIdleAndHours(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	facts := BuildDayFacts([]periodEvent{
		pevent(clock(start, 10, 0, 0), "task_added", nil),
		pevent(clock(start, 10, 7, 0), "task_added", nil),
		pevent(clock(start, 10, 20, 0), "outgoing_call", number(150)),
	}, day, loc)
	w := periodWindow{540, 1080}
	stat := DayStatFrom(facts, w, nil, nil)
	if *stat.First != 600 || *stat.Last != 624 || stat.IdleInside != 4+12 || stat.Active != 8 || stat.Call != 4 {
		t.Fatalf("first/last/idle %+v", stat)
	}
	cut := 607
	stat = DayStatFrom(facts, w, &cut, nil)
	if *stat.First != 600 || *stat.Last != 602 || stat.IdleInside != 0 || stat.Elapsed != 67 {
		t.Fatalf("cut tail must not count as idle: %+v", stat)
	}
	cut = 619
	stat = DayStatFrom(BuildDayFacts([]periodEvent{pevent(clock(start, 10, 18, 0), "task_added", nil)}, day, loc), w, &cut, nil)
	if *stat.Last != 619 || *stat.First != 618 {
		t.Fatalf("last is clipped to the range end: %+v", stat)
	}
	if empty := DayStatFrom(DayFacts{Day: day}, w, nil, nil); empty.First != nil || empty.Last != nil || empty.Active != 0 || empty.Elapsed != 540 {
		t.Fatalf("empty %+v", empty)
	}
	// An odd window start puts segment [599,601) into hour 9 whole; the hour
	// share may exceed 1 and is clamped by the client.
	w = periodWindow{541, 661}
	if hours := w.hours(); !reflect.DeepEqual(hours, []int{9, 10, 11}) {
		t.Fatalf("hours %v", hours)
	}
	stat = DayStatFrom(BuildDayFacts([]periodEvent{
		pevent(clock(start, 9, 59, 0), "task_added", nil),
		pevent(clock(start, 11, 0, 0), "outgoing_call", number(30)),
	}, day, loc), w, nil, nil)
	if !reflect.DeepEqual(stat.Hours, [][3]int{{2, 0, 59}, {2, 2, 60}, {0, 0, 1}}) {
		t.Fatalf("hours %v", stat.Hours)
	}
	cut = 10*60 + 30
	stat = DayStatFrom(DayFacts{}, periodWindow{540, 1080}, &cut, nil)
	if len(stat.Hours) != 9 || stat.Hours[0] != [3]int{0, 0, 60} || stat.Hours[1] != [3]int{0, 0, 30} || stat.Hours[2] != [3]int{0, 0, 0} {
		t.Fatalf("cut hours %v", stat.Hours)
	}
	if !reflect.DeepEqual(periodWindow{0, 1440}.hours(), []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}) || !reflect.DeepEqual(periodWindow{545, 546}.hours(), []int{9}) {
		t.Fatal("window hours")
	}
}

// dashboard-v1 quirk kept for parity: a cut in the hour that starts at the
// window end extends the range past it; hours and segment strings stay
// within the window.
func TestPeriodCutPastWindowEndKeepsDashboardRule(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	cutoff := clock(start, 18, 30, 0)
	events := []periodEvent{pevent(clock(start, 18, 10, 0), "task_added", nil), pevent(clock(start, 17, 59, 0), "task_added", nil)}
	stat := assertDashboardParity(t, events, day, loc, periodWindow{540, 1080}, &cutoff)
	kinds, zones := stat.segments()
	if stat.Elapsed != 570 || stat.Active != 4 || *stat.Last != 1092 || len(stat.Hours) != 9 || stat.Hours[8] != [3]int{2, 0, 60} || len(kinds) != 270 || kinds[269] != '1' || zones[269] != 'T' {
		t.Fatalf("stat %+v kinds %q", stat, kinds[260:])
	}
	cutoff = clock(start, 19, 0, 0)
	if stat = assertDashboardParity(t, events, day, loc, periodWindow{540, 1080}, &cutoff); stat.Elapsed != 540 {
		t.Fatalf("next hour returns to the window end: %+v", stat)
	}
}

func TestPeriodCountedWeekend(t *testing.T) {
	loc, saturday := periodTestDay(t, "2026-10-31")
	w := periodWindow{540, 1080}
	if DayStatFrom(DayFacts{}, w, nil, nil).counted(saturday) {
		t.Fatal("weekend without activity counted")
	}
	outside := BuildDayFacts([]periodEvent{pevent(clock(saturday, 20, 0, 0), "task_added", nil)}, "2026-10-31", loc)
	if DayStatFrom(outside, w, nil, nil).counted(saturday) {
		t.Fatal("weekend activity outside the window counted")
	}
	inside := BuildDayFacts([]periodEvent{pevent(clock(saturday, 12, 0, 0), "task_added", nil)}, "2026-10-31", loc)
	if !DayStatFrom(inside, w, nil, nil).counted(saturday) {
		t.Fatal("weekend with activity not counted")
	}
	_, monday := periodTestDay(t, "2026-11-02")
	if !DayStatFrom(DayFacts{}, w, nil, nil).counted(monday) {
		t.Fatal("empty weekday must count with zero")
	}
}

func TestPeriodPipelineSlicesMatchRawFilter(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	payloads := []string{``, `[]`, `[{"lead_status":{"pipeline_id":10}}]`, `[{"pipeline_id":"11"}]`, `[{"pipeline":{"id":11}},{"pipeline":{"id":10}}]`, `[{"options":{"pipeline":{"id":12}}}]`}
	rng := rand.New(rand.NewSource(7))
	raw := make([]serviceapi.Event, 0, 300)
	for i := 0; i < 300; i++ {
		e := serviceapi.Event{CreatedAt: clock(start, 9, rng.Intn(600), 0), Type: []string{"lead_status_changed", "task_completed", "outgoing_call"}[rng.Intn(3)], EntityType: "lead"}
		if p := payloads[rng.Intn(len(payloads))]; p != "" {
			e.ValueAfter = json.RawMessage(p)
		}
		raw = append(raw, e)
	}
	all := make([]periodEvent, 0, len(raw))
	for _, e := range raw {
		all = append(all, periodEventFromAPI(e))
	}
	facts := BuildDayFacts(all, day, loc)
	keys := [][]string{}
	for _, s := range facts.Slices {
		keys = append(keys, s.Pipelines)
	}
	if !reflect.DeepEqual(keys, [][]string{nil, {"10"}, {"10", "11"}, {"11"}, {"12"}}) {
		t.Fatalf("slices %v", keys)
	}
	w := periodWindow{540, 1080}
	for _, selected := range [][]int64{nil, {10}, {11}, {12}, {13}, {10, 12}} {
		var filtered []periodEvent
		for _, e := range raw {
			if ratingPipelineMatch(e, selected) {
				filtered = append(filtered, periodEventFromAPI(e))
			}
		}
		got := DayStatFrom(facts, w, nil, selected)
		want := DayStatFrom(BuildDayFacts(filtered, day, loc), w, nil, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("selected %v: slices %+v raw %+v", selected, got.KPI, want.KPI)
		}
		if roundedRating(float64(got.Active), float64(got.Elapsed)) != dashboardRating(ratingEvents(filtered), day, loc, w.Start, w.End, nil) {
			t.Fatalf("selected %v: dashboard parity", selected)
		}
	}
}

// 138 active of 480 elapsed is the JavaScript half-tie: the dashboard shows
// 28.7, while round(active/elapsed*1000)/10 would give 28.8.
func TestPeriodParityHalfTie(t *testing.T) {
	const day = "2026-10-26"
	loc, start := periodTestDay(t, day)
	events := make([]periodEvent, 0, 69)
	for i := 0; i < 69; i++ {
		events = append(events, pevent(clock(start, 9, 2*i, 0), "task_added", nil))
	}
	stat := assertDashboardParity(t, events, day, loc, periodWindow{540, 1020}, nil)
	if stat.Active != 138 || stat.Elapsed != 480 || dashboardRating(ratingEvents(events), day, loc, 540, 1020, nil) != 28.7 {
		t.Fatalf("half tie %d/%d", stat.Active, stat.Elapsed)
	}
}
