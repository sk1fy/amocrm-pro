package serviceapi

import (
	"context"
	"time"
)

// These versions deliberately retain the two distinct legacy UI formulas.
const DashboardActivityFormula = "dashboard-v1"
const ProfileActivityFormula = "profile-v1"
const ActivityRatingMaxPages = 60

type ActivityRatingRequest struct {
	Auth         Auth    `json:"auth"`
	From         int64   `json:"from"`
	To           int64   `json:"to"`
	Day          string  `json:"day"`
	Surface      string  `json:"surface"`
	StartMinutes int     `json:"start_minutes"`
	EndMinutes   int     `json:"end_minutes"`
	CutoffUnix   *int64  `json:"cutoff_unix,omitempty"`
	EmployeeIDs  []int64 `json:"employee_ids,omitempty"`
}
type EmployeeActivityRating struct {
	EmployeeID int64   `json:"employee_id"`
	Percent    float64 `json:"percent"`
	// Profile only: ready percentages for local clock minutes 0..1440. Selecting
	// a value as the UI clock ticks does not repeat the business calculation.
	MinutePercentages []float64 `json:"minute_percentages,omitempty"`
}
type ActivityRatings struct {
	FormulaVersion string                   `json:"formula_version"`
	Day            string                   `json:"day"`
	Timezone       string                   `json:"timezone"`
	StartMinutes   int                      `json:"start_minutes"`
	EndMinutes     int                      `json:"end_minutes"`
	CutoffUnix     *int64                   `json:"cutoff_unix,omitempty"`
	SnapshotAt     int64                    `json:"snapshot_at"`
	Partial        bool                     `json:"partial"`
	Employees      []EmployeeActivityRating `json:"employees"`
}

// Additive port: old clients fail explicitly rather than calculate in the UI.
type ActivityRatingReader interface {
	ViewActivityRatings(context.Context, ActivityRatingRequest) (ActivityRatings, error)
}

func ValidateActivityRatingRequest(r ActivityRatingRequest) error {
	day, err := time.Parse("2006-01-02", r.Day)
	if err != nil || day.Format("2006-01-02") != r.Day || r.From < 0 || r.To <= r.From || r.To-r.From > 26*3600 || r.StartMinutes < 0 || r.EndMinutes > 1440 || r.EndMinutes <= r.StartMinutes || (r.Surface != "dashboard" && r.Surface != "profile") || (r.CutoffUnix != nil && *r.CutoffUnix < 0) || len(r.EmployeeIDs) > 100 {
		return Fail(InvalidArgument, "invalid activity rating day, period, surface or window")
	}
	if r.Surface == "profile" && len(r.EmployeeIDs) != 1 {
		return Fail(InvalidArgument, "profile activity requires one employee")
	}
	seen := map[int64]bool{}
	for _, id := range r.EmployeeIDs {
		if id <= 0 || seen[id] {
			return Fail(InvalidArgument, "invalid activity rating employees")
		}
		seen[id] = true
	}
	return nil
}
