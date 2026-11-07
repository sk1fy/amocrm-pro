package activity

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// period-v1 (docs/proposals/activity-period-v1.md §2–§3). Day facts do not
// depend on the window or the panel pipeline filter; a day statistic is read
// from facts, a window and an optional cutoff. Occupancy and segments follow
// dashboardRating exactly, so a day statistic reproduces the dashboard-v1 rating.

const periodSegmentMinutes = 2

// Zone ranks: a minute keeps the highest-ranked zone of the events occupying it.
const (
	zoneNone uint8 = iota
	zoneOther
	zoneLeads
	zoneImbox
	zoneTasks
	zoneCalls
)

// zoneCodes indexes the wire codes of period-days `zones` by rank.
var zoneCodes = [...]byte{'-', 'O', 'D', 'I', 'T', 'C'}

// KPI keys in wire order: outgoing_calls, messages, outgoing_mail,
// completed_tasks, lead_status_changes.
const periodKPICount = 5

func periodKPI(eventType string) int {
	switch {
	case eventType == "outgoing_call":
		return 0
	case periodMessage(eventType):
		return 1
	case eventType == "outgoing_mail":
		return 2
	case eventType == "task_completed":
		return 3
	case eventType == "lead_status_changed":
		return 4
	}
	return -1
}

func periodMessage(eventType string) bool {
	return eventType == "entity_direct_message" || strings.Contains(eventType, "chat_message") || strings.Contains(eventType, "sms")
}

func periodZone(eventType, entityType string) uint8 {
	switch {
	case strings.Contains(eventType, "call"):
		return zoneCalls
	case strings.HasPrefix(eventType, "task_") || entityType == "task" || entityType == "tasks":
		return zoneTasks
	case periodMessage(eventType):
		return zoneImbox
	case entityType == "lead" || entityType == "leads":
		return zoneLeads
	}
	return zoneOther
}

type periodEvent struct {
	ratingEvent
	EntityType string
	// Sorted pipeline ID set exactly as ratingPipelineMatch extracts it.
	Pipelines []string
}

func periodEventFromAPI(e serviceapi.Event) periodEvent {
	return periodEvent{ratingEvent: ratingEventFromAPI(e), EntityType: e.EntityType, Pipelines: ratingPipelineIDs(e)}
}

// DayFacts are one employee's facts for one calendar day in the account timezone.
type DayFacts struct {
	Day                  string
	Slices               []DaySlice
	CallsWithoutDuration int
}

// DaySlice holds the facts of the events sharing one pipeline ID set; an empty
// set is the slice of events without pipeline attributes.
type DaySlice struct {
	Pipelines []string
	// Zone rank per local minute 0..1439; zoneNone is a free minute and
	// zoneCalls is a minute occupied by a call.
	Minutes [1440]uint8
	// Local minute of every KPI event, one entry per event.
	KPI [periodKPICount][]int16
}

func (f *DayFacts) slice(pipelines []string) *DaySlice {
	for i := range f.Slices {
		if slices.Equal(f.Slices[i].Pipelines, pipelines) {
			return &f.Slices[i]
		}
	}
	f.Slices = append(f.Slices, DaySlice{Pipelines: pipelines})
	return &f.Slices[len(f.Slices)-1]
}

// BuildDayFacts keeps only events of the given local day, as dashboardRating does.
func BuildDayFacts(events []periodEvent, day string, loc *time.Location) DayFacts {
	facts := DayFacts{Day: day}
	for _, e := range events {
		at := time.Unix(e.At, 0).In(loc)
		if at.Format(time.DateOnly) != day {
			continue
		}
		m := at.Hour()*60 + at.Minute()
		end := m + 1
		if strings.Contains(e.Type, "call") && e.Duration != nil && *e.Duration > 0 {
			if length := math.Ceil(*e.Duration / 60); float64(m)+length < 1440 {
				end = m + int(length)
			} else {
				end = 1440
			}
		}
		if (e.Type == "incoming_call" || e.Type == "outgoing_call") && e.Duration == nil {
			facts.CallsWithoutDuration++
		}
		s := facts.slice(e.Pipelines)
		zone := periodZone(e.Type, e.EntityType)
		for minute := m; minute < min(end, 1440); minute++ {
			s.Minutes[minute] = max(s.Minutes[minute], zone)
		}
		if k := periodKPI(e.Type); k >= 0 {
			s.KPI[k] = append(s.KPI[k], int16(m))
		}
	}
	slices.SortFunc(facts.Slices, func(a, b DaySlice) int { return slices.Compare(a.Pipelines, b.Pipelines) })
	return facts
}

// merge joins the slices that pass the panel filter; the result equals facts
// built from raw events filtered by ratingPipelineMatch.
func (f DayFacts) merge(selected []int64) (minutes [1440]uint8, kpi [periodKPICount][]int16) {
	for i := range f.Slices {
		s := &f.Slices[i]
		if !pipelineSetMatch(s.Pipelines, selected) {
			continue
		}
		for m, zone := range s.Minutes {
			minutes[m] = max(minutes[m], zone)
		}
		for k := range kpi {
			kpi[k] = append(kpi[k], s.KPI[k]...)
		}
	}
	return minutes, kpi
}

type periodWindow struct{ Start, End int }

// hours lists every clock hour the window touches.
func (w periodWindow) hours() []int {
	out := make([]int, 0, 24)
	for h := w.Start / 60; h*60 < w.End; h++ {
		out = append(out, h)
	}
	return out
}

// rangeEnd applies the dashboard-v1 cutoff rule; cut is a local clock minute.
// Kept bit-exact for parity: a cut inside hour floor(end/60) yields a range
// end past the window end (e.g. window 09:00–18:00, cut 18:30 → 18:30).
func (w periodWindow) rangeEnd(cut *int) int {
	if cut == nil {
		return w.End
	}
	cutHour := *cut / 60
	h := min(w.End/60, cutHour)
	end := h * 60
	if cutHour == h {
		end += *cut % 60
	}
	return end
}

type dayStat struct {
	KPI                               [periodKPICount]int
	Events                            int
	Active, Call, Elapsed, IdleInside int
	ActiveSegments                    int
	First, Last                       *int
	Hours                             [][3]int
	minutes                           [1440]uint8
	rangeEnd                          int
	window                            periodWindow
}

// DayStatFrom reads one day statistic: 2-minute segments from the window start
// to the range end (the last one may be shorter); a segment is active when any
// minute is occupied and a call segment when any minute is a call.
func DayStatFrom(facts DayFacts, w periodWindow, cut *int, selected []int64) dayStat {
	minutes, kpi := facts.merge(selected)
	end := w.rangeEnd(cut)
	stat := dayStat{Elapsed: max(0, end-w.Start), minutes: minutes, rangeEnd: end, window: w}
	hours := w.hours()
	stat.Hours = make([][3]int, len(hours))
	for i, h := range hours {
		stat.Hours[i][2] = max(0, min(end, w.End, h*60+60)-max(w.Start, h*60))
	}
	idle := 0
	for left := w.Start; left < end; left += periodSegmentMinutes {
		right := min(left+periodSegmentMinutes, end)
		zone := zoneNone
		hourZone := zoneNone
		for m := left; m < right; m++ {
			zone = max(zone, minutes[m])
			if m < w.End {
				hourZone = max(hourZone, minutes[m])
			}
		}
		if zone == zoneNone {
			if stat.First != nil {
				idle++
			}
			continue
		}
		stat.ActiveSegments++
		// dashboard-v1 lets a cutoff in hour floor(end/60) extend the range
		// past the window end; those segments have no column in hours.
		hour := left/60 - w.Start/60
		if hour < len(stat.Hours) && hourZone != zoneNone {
			length := min(right, w.End) - left
			stat.Hours[hour][0] += length
			if hourZone == zoneCalls {
				stat.Hours[hour][1] += length
			}
		}
		if zone == zoneCalls {
			stat.Call += periodSegmentMinutes
		}
		if stat.First == nil {
			stat.First = &left
		}
		stat.Last = &right
		stat.IdleInside += idle * periodSegmentMinutes
		idle = 0
	}
	stat.Active = min(stat.Elapsed, stat.ActiveSegments*periodSegmentMinutes)
	stat.Call = min(stat.Elapsed, stat.Call)
	for k, list := range kpi {
		for _, m := range list {
			if int(m) >= w.Start && int(m) < end {
				stat.KPI[k]++
			}
		}
		stat.Events += stat.KPI[k]
	}
	return stat
}

// counted applies R4: a weekend day without active segments stays out of sums.
func (s dayStat) counted(day time.Time) bool {
	weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
	return !weekend || s.ActiveSegments > 0
}

// segments renders period-days strings for the whole window; segments at or
// after the range end have not happened yet.
func (s dayStat) segments() (kinds, zones string) {
	n := (s.window.End - s.window.Start + 1) / periodSegmentMinutes
	k := make([]byte, n)
	z := make([]byte, n)
	for i := range n {
		left := s.window.Start + i*periodSegmentMinutes
		if left >= s.rangeEnd {
			k[i], z[i] = '.', '.'
			continue
		}
		zone := zoneNone
		for m := left; m < min(left+periodSegmentMinutes, s.rangeEnd); m++ {
			zone = max(zone, s.minutes[m])
		}
		z[i] = zoneCodes[zone]
		switch zone {
		case zoneNone:
			k[i] = '0'
		case zoneCalls:
			k[i] = '2'
		default:
			k[i] = '1'
		}
	}
	return string(k), string(z)
}
