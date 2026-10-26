package activitybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type ratingHTTPProduct struct {
	shareActivity
	request serviceapi.ActivityRatingRequest
}

func (p *ratingHTTPProduct) ViewActivityRatings(_ context.Context, r serviceapi.ActivityRatingRequest) (serviceapi.ActivityRatings, error) {
	p.request = r
	return serviceapi.ActivityRatings{FormulaVersion: serviceapi.DashboardActivityFormula, Day: r.Day, Timezone: "Europe/Moscow", StartMinutes: r.StartMinutes, EndMinutes: r.EndMinutes, CutoffUnix: r.CutoffUnix, Employees: []serviceapi.EmployeeActivityRating{{EmployeeID: 7, Percent: 40}}}, nil
}
func TestActivityRatingHTTPQueryAndViewerAccess(t *testing.T) {
	p := &ratingHTTPProduct{shareActivity: shareActivity{lookup: serviceapi.ShareLookup{Enabled: true, PanelID: uuid.New(), ViewKeyVersion: 1}}}
	policy := &sharePolicy{}
	b := New(nil, policy, p, nil)
	b.ConfigureShare([]string{"http://localhost:5174"}, "")
	router := chi.NewRouter()
	b.RegisterShareHTTP(router)
	query := "/api/v1/activity/view/activity?from=1792962000&to=1792994400&day=2026-10-26&surface=dashboard&start_minutes=540&end_minutes=1080&cutoff_unix=1792976400&employee_ids=7,8"
	request := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	w := request(query, "synthetic-view-key")
	if w.Code != 200 {
		t.Fatalf("ratingHTTP%d %s", w.Code, w.Body.String())
	}
	var got serviceapi.ActivityRatings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FormulaVersion != serviceapi.DashboardActivityFormula || len(got.Employees) != 1 || got.Employees[0].Percent != 40 || p.request.Auth.Token != "delegated" || len(p.request.EmployeeIDs) != 2 || p.request.CutoffUnix == nil || *p.request.CutoffUnix != 1792976400 || policy.kind != serviceapi.PrincipalKindViewer {
		t.Fatalf("response%+v request%+v", got, p.request)
	}
	for _, suffix := range []string{"&surprise=1", "&day=2026-10-27", "&employee_ids=7", "&cutoff_unix=bad"} {
		if w := request(query+suffix, "synthetic-view-key"); w.Code != 400 {
			t.Fatalf("query%s=%d", suffix, w.Code)
		}
	}
	if w := request(query, "unknown"); w.Code != 404 {
		t.Fatalf("unknown viewer=%d", w.Code)
	}
}
