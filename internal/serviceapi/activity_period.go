package serviceapi

import (
	"context"
	"time"
)

// Week/month statistics (docs/proposals/activity-period-v1.md). Activity owns
// the formula; CRM Events supplies raw facts only.
const ActivityPeriodFormula = "period-v1"

// The period batch has the rating deadline (below the 30-second delegation
// lifetime). Event scanning stops after ActivityPeriodScanBudget and marks the
// unscanned rest of a day partial instead of failing the response.
const ActivityPeriodTimeout = ActivityRatingTimeout
const ActivityPeriodScanBudget = 20 * time.Second

// Each day is scanned with the rating page bound (60 x 100 events for the
// whole employee batch); at most ActivityPeriodScanWorkers days in parallel.
const ActivityPeriodDayMaxPages = ActivityRatingMaxPages
const ActivityPeriodScanWorkers = 4

// Calls without a known duration fall back to their event card, as in the
// rating: at most this many per request.
const ActivityPeriodMaxCards = 150

// period-days covers at most 31 days; history is never deeper than retention.
const ActivityPeriodDaysMax = 31
const ActivityPeriodHistoryDays = RetentionDaysMax
const ActivityPeriodSegmentMinutes = 2

const (
	ActivityPeriodWeek  = "week"
	ActivityPeriodMonth = "month"
)

const (
	ActivityDayFuture      = "future"
	ActivityDayToday       = "today"
	ActivityDayReady       = "ready"
	ActivityDayPending     = "pending"
	ActivityDayUnavailable = "unavailable"
)

type ActivityPeriodRequest struct {
	Auth         Auth    `json:"auth"`
	Kind         string  `json:"kind"`
	Anchor       string  `json:"anchor"`
	StartMinutes int     `json:"start_minutes"`
	EndMinutes   int     `json:"end_minutes"`
	CutoffUnix   *int64  `json:"cutoff_unix,omitempty"`
	EmployeeIDs  []int64 `json:"employee_ids,omitempty"`
}

type ActivityPeriodRange struct {
	Start string   `json:"start"`
	End   string   `json:"end"`
	Days  []string `json:"days"`
}

// ActivityPeriodCut is "up to the same point": DayIndex is today's index in
// the current period, Minute the local clock minute of the cutoff.
type ActivityPeriodCut struct {
	DayIndex int `json:"day_index"`
	Minute   int `json:"minute"`
}

type ActivityPreviousPeriod struct {
	ActivityPeriodRange
	Available bool               `json:"available"`
	Cut       *ActivityPeriodCut `json:"cut"`
}

// ActivityDayStat carries only day, state and counted for future, pending and
// unavailable days; the embedded values are present for ready and today.
type ActivityDayStat struct {
	Day     string `json:"day"`
	State   string `json:"state"`
	Counted bool   `json:"counted"`
	*ActivityDayValues
}

type ActivityDayValues struct {
	Coverage string `json:"coverage"`
	Partial  bool   `json:"partial"`
	// [outgoing_calls, messages, outgoing_mail, completed_tasks, lead_status_changes]
	KPI        []int `json:"kpi"`
	Events     int   `json:"events"`
	Active     int   `json:"active"`
	Call       int   `json:"call"`
	Elapsed    int   `json:"elapsed"`
	IdleInside int   `json:"idle_inside"`
	First      *int  `json:"first"`
	Last       *int  `json:"last"`
	// One [active, call, elapsed] triple per response hour.
	Hours [][3]int `json:"hours"`
}

type ActivityPeriodCurrent struct {
	Rating *float64          `json:"rating"`
	Days   []ActivityDayStat `json:"days"`
}

type ActivityPeriodPrevious struct {
	Rating     *float64          `json:"rating"`
	RatingFull *float64          `json:"rating_full"`
	Days       []ActivityDayStat `json:"days"`
	CutDay     *ActivityDayStat  `json:"cut_day"`
}

type EmployeeActivityPeriod struct {
	EmployeeID int64                   `json:"employee_id"`
	Current    ActivityPeriodCurrent   `json:"current"`
	Previous   *ActivityPeriodPrevious `json:"previous"`
}

type ActivityPeriod struct {
	FormulaVersion string                   `json:"formula_version"`
	Kind           string                   `json:"kind"`
	Timezone       string                   `json:"timezone"`
	StartMinutes   int                      `json:"start_minutes"`
	EndMinutes     int                      `json:"end_minutes"`
	CutoffUnix     int64                    `json:"cutoff_unix"`
	SnapshotAt     int64                    `json:"snapshot_at"`
	HistoryFrom    string                   `json:"history_from"`
	RefreshAfter   *int64                   `json:"refresh_after,omitempty"`
	Hours          []int                    `json:"hours"`
	Current        ActivityPeriodRange      `json:"current"`
	Previous       ActivityPreviousPeriod   `json:"previous"`
	Employees      []EmployeeActivityPeriod `json:"employees"`
}

type ActivityPeriodDaysRequest struct {
	Auth         Auth   `json:"auth"`
	EmployeeID   int64  `json:"employee_id"`
	FromDay      string `json:"from_day"`
	ToDay        string `json:"to_day"`
	StartMinutes int    `json:"start_minutes"`
	EndMinutes   int    `json:"end_minutes"`
	CutoffUnix   *int64 `json:"cutoff_unix,omitempty"`
}

// ActivityPeriodDay carries only day and state for future, pending and
// unavailable days.
type ActivityPeriodDay struct {
	Day   string `json:"day"`
	State string `json:"state"`
	*ActivityPeriodDaySegments
}

type ActivityPeriodDaySegments struct {
	Coverage string `json:"coverage"`
	Partial  bool   `json:"partial"`
	First    *int   `json:"first"`
	Last     *int   `json:"last"`
	// One character per 2-minute window segment: kinds 0 pause, 1 CRM,
	// 2 call; zones - none, C/T/I/D/O; "." not yet happened.
	Kinds string `json:"kinds"`
	Zones string `json:"zones"`
}

type ActivityPeriodDays struct {
	FormulaVersion string              `json:"formula_version"`
	Timezone       string              `json:"timezone"`
	EmployeeID     int64               `json:"employee_id"`
	StartMinutes   int                 `json:"start_minutes"`
	EndMinutes     int                 `json:"end_minutes"`
	SegmentMinutes int                 `json:"segment_minutes"`
	CutoffUnix     int64               `json:"cutoff_unix"`
	SnapshotAt     int64               `json:"snapshot_at"`
	HistoryFrom    string              `json:"history_from"`
	Days           []ActivityPeriodDay `json:"days"`
}

// Additive port, like ActivityRatingReader.
type ActivityPeriodReader interface {
	ViewActivityPeriod(context.Context, ActivityPeriodRequest) (ActivityPeriod, error)
	ViewActivityPeriodDays(context.Context, ActivityPeriodDaysRequest) (ActivityPeriodDays, error)
}

func validDay(value string) bool {
	day, err := time.Parse(time.DateOnly, value)
	return err == nil && day.Format(time.DateOnly) == value
}

func validWindow(start, end int, cutoff *int64) bool {
	return start >= 0 && start <= 1439 && end >= 1 && end <= 1440 && end > start && (cutoff == nil || *cutoff >= 0)
}

func ValidateActivityPeriodRequest(r ActivityPeriodRequest) error {
	if (r.Kind != ActivityPeriodWeek && r.Kind != ActivityPeriodMonth) || !validDay(r.Anchor) || !validWindow(r.StartMinutes, r.EndMinutes, r.CutoffUnix) || len(r.EmployeeIDs) > 100 {
		return Fail(InvalidArgument, "invalid activity period kind, anchor, window, cutoff or employees")
	}
	seen := map[int64]bool{}
	for _, id := range r.EmployeeIDs {
		if id <= 0 || seen[id] {
			return Fail(InvalidArgument, "invalid activity period employees")
		}
		seen[id] = true
	}
	return nil
}

func ValidateActivityPeriodDaysRequest(r ActivityPeriodDaysRequest) error {
	if r.EmployeeID <= 0 || !validDay(r.FromDay) || !validDay(r.ToDay) || !validWindow(r.StartMinutes, r.EndMinutes, r.CutoffUnix) {
		return Fail(InvalidArgument, "invalid activity period days employee, range, window or cutoff")
	}
	from, _ := time.Parse(time.DateOnly, r.FromDay)
	to, _ := time.Parse(time.DateOnly, r.ToDay)
	if to.Before(from) || to.Sub(from) >= ActivityPeriodDaysMax*24*time.Hour {
		return Fail(InvalidArgument, "activity period days range must be 1..31 days")
	}
	return nil
}
