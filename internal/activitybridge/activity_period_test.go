package activitybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type periodHTTPProduct struct {
	shareActivity
	period    serviceapi.ActivityPeriodRequest
	days      serviceapi.ActivityPeriodDaysRequest
	remaining time.Duration
	fail      error
}

func (p *periodHTTPProduct) ViewActivityPeriod(ctx context.Context, r serviceapi.ActivityPeriodRequest) (serviceapi.ActivityPeriod, error) {
	p.period = r
	deadline, _ := ctx.Deadline()
	p.remaining = time.Until(deadline)
	if p.fail != nil {
		return serviceapi.ActivityPeriod{}, p.fail
	}
	return serviceapi.ActivityPeriod{FormulaVersion: serviceapi.ActivityPeriodFormula, Kind: r.Kind, StartMinutes: r.StartMinutes, EndMinutes: r.EndMinutes, CutoffUnix: *r.CutoffUnix, Hours: []int{9}, Employees: []serviceapi.EmployeeActivityPeriod{{EmployeeID: 7, Current: serviceapi.ActivityPeriodCurrent{Days: []serviceapi.ActivityDayStat{{Day: "2026-11-08", State: "future"}}}}}}, nil
}

func (p *periodHTTPProduct) ViewActivityPeriodDays(ctx context.Context, r serviceapi.ActivityPeriodDaysRequest) (serviceapi.ActivityPeriodDays, error) {
	p.days = r
	deadline, _ := ctx.Deadline()
	p.remaining = time.Until(deadline)
	return serviceapi.ActivityPeriodDays{FormulaVersion: serviceapi.ActivityPeriodFormula, EmployeeID: r.EmployeeID, SegmentMinutes: 2, Days: []serviceapi.ActivityPeriodDay{{Day: r.FromDay, State: "ready", ActivityPeriodDaySegments: &serviceapi.ActivityPeriodDaySegments{Kinds: "012", Zones: "-CT"}}}}, nil
}

func periodRouter(t *testing.T, p serviceapi.Activity) (*sharePolicy, func(path, key string) *httptest.ResponseRecorder) {
	t.Helper()
	policy := &sharePolicy{}
	b := New(nil, policy, p, nil)
	b.ConfigureShare([]string{"http://localhost:5174"}, "")
	router := chi.NewRouter()
	b.RegisterShareHTTP(router)
	return policy, func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
}

func TestActivityPeriodHTTPQueryAndViewerAccess(t *testing.T) {
	p := &periodHTTPProduct{shareActivity: shareActivity{lookup: serviceapi.ShareLookup{Enabled: true, PanelID: uuid.New(), ViewKeyVersion: 1}}}
	policy, request := periodRouter(t, p)
	query := "/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1080&cutoff_unix=1793000000&employee_ids=7,8"
	w := request(query, "synthetic-view-key")
	if w.Code != 200 {
		t.Fatalf("period HTTP %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	day := got["employees"].([]any)[0].(map[string]any)["current"].(map[string]any)["days"].([]any)[0].(map[string]any)
	if got["formula_version"] != "period-v1" || got["cutoff_unix"] != float64(1793000000) || len(day) != 3 || day["counted"] != false {
		t.Fatalf("response %s", w.Body.String())
	}
	if p.remaining < 24*time.Second || p.remaining > 25*time.Second {
		t.Fatalf("period HTTP deadline=%v", p.remaining)
	}
	if p.period.Auth.Token != "delegated" || p.period.Kind != "week" || p.period.Anchor != "2026-11-05" || !slices.Equal(p.period.EmployeeIDs, []int64{7, 8}) || p.period.StartMinutes != 540 || p.period.EndMinutes != 1080 || policy.kind != serviceapi.PrincipalKindViewer {
		t.Fatalf("request %+v", p.period)
	}
	// cutoff_unix and employee_ids are optional.
	if w := request("/api/v1/activity/view/period?kind=month&anchor=2026-11-05&start_minutes=0&end_minutes=1440&cutoff_unix=1", "synthetic-view-key"); w.Code != 200 || p.period.EmployeeIDs != nil || p.period.Kind != "month" {
		t.Fatalf("minimal period %d %+v", w.Code, p.period)
	}
	for _, bad := range []string{
		query + "&surprise=1",
		query + "&kind=week",
		"/api/v1/activity/view/period?kind=day&anchor=2026-11-05&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/period?kind=week&anchor=2026-02-30&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/period?kind=week&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=1080&end_minutes=1080",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1441",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1080&cutoff_unix=bad",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1080&employee_ids=7,x",
		"/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1080&employee_ids=7,7",
	} {
		if w := request(bad, "synthetic-view-key"); w.Code != 400 {
			t.Fatalf("%s = %d", bad, w.Code)
		}
	}
	if w := request(query, "unknown"); w.Code != 404 {
		t.Fatalf("unknown viewer=%d", w.Code)
	}
	p.fail = serviceapi.Fail(serviceapi.NotFound, "not found")
	if w := request(query, "synthetic-view-key"); w.Code != 404 {
		t.Fatalf("foreign employee=%d", w.Code)
	}
	p.fail = serviceapi.Fail(serviceapi.ResourceExhausted, "response exceeds size limit")
	if w := request(query, "synthetic-view-key"); w.Code != 429 {
		t.Fatalf("size limit=%d", w.Code)
	}
}

func TestActivityPeriodDaysHTTP(t *testing.T) {
	p := &periodHTTPProduct{shareActivity: shareActivity{lookup: serviceapi.ShareLookup{Enabled: true, PanelID: uuid.New(), ViewKeyVersion: 1}}}
	_, request := periodRouter(t, p)
	query := "/api/v1/activity/view/employees/7/period-days?from_day=2026-11-02&to_day=2026-11-08&start_minutes=540&end_minutes=1080&cutoff_unix=1793000000"
	w := request(query, "synthetic-view-key")
	if w.Code != 200 {
		t.Fatalf("period-days HTTP %d %s", w.Code, w.Body.String())
	}
	var got serviceapi.ActivityPeriodDays
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EmployeeID != 7 || got.Days[0].Kinds != "012" || p.days.EmployeeID != 7 || p.days.FromDay != "2026-11-02" || p.days.ToDay != "2026-11-08" || p.days.CutoffUnix == nil || *p.days.CutoffUnix != 1793000000 || p.days.Auth.Token != "delegated" {
		t.Fatalf("response %+v request %+v", got, p.days)
	}
	if p.remaining < 24*time.Second || p.remaining > 25*time.Second {
		t.Fatalf("period-days HTTP deadline=%v", p.remaining)
	}
	for _, bad := range []string{
		query + "&employee_ids=7",
		"/api/v1/activity/view/employees/7/period-days?from_day=2026-10-01&to_day=2026-11-01&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/employees/7/period-days?from_day=2026-11-08&to_day=2026-11-02&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/employees/7/period-days?from_day=2026-11-02&start_minutes=540&end_minutes=1080",
		"/api/v1/activity/view/employees/7/period-days?from_day=2026-11-02&to_day=2026-11-08&start_minutes=-1&end_minutes=1080",
	} {
		if w := request(bad, "synthetic-view-key"); w.Code != 400 {
			t.Fatalf("%s = %d", bad, w.Code)
		}
	}
	if w := request("/api/v1/activity/view/employees/0/period-days?from_day=2026-11-02&to_day=2026-11-08&start_minutes=540&end_minutes=1080", "synthetic-view-key"); w.Code != 404 {
		t.Fatalf("bad employee path=%d", w.Code)
	}
	if w := request(query, "unknown"); w.Code != 404 {
		t.Fatalf("unknown viewer=%d", w.Code)
	}
}

func TestActivityPeriodHTTPWithoutPort(t *testing.T) {
	_, request := periodRouter(t, &shareActivity{lookup: serviceapi.ShareLookup{Enabled: true, PanelID: uuid.New(), ViewKeyVersion: 1}})
	if w := request("/api/v1/activity/view/period?kind=week&anchor=2026-11-05&start_minutes=540&end_minutes=1080", "synthetic-view-key"); w.Code != 503 {
		t.Fatalf("missing period port=%d", w.Code)
	}
}
