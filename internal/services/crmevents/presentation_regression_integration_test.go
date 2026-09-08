package crmevents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
)

type filteredDirectory struct{ testGateway }

func (*filteredDirectory) Users(_ context.Context, r serviceapi.UsersRequest) (serviceapi.Directory, error) {
	d := serviceapi.Directory{Timezone: "UTC"}
	for i, id := range []int64{7, 9} {
		selected := len(r.UserIDs) == 0
		for _, want := range r.UserIDs {
			if want == id {
				selected = true
			}
		}
		if selected {
			d.Users = append(d.Users, serviceapi.User{ID: id, GroupID: int64(i + 1)})
		}
	}
	return d, nil
}

type presentationSettings struct{ activity.Repository }

func (presentationSettings) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	return serviceapi.DefaultSettings(), nil
}
func TestPanelSelectedAndUnknownAuthorsWithFilteredGateway(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	for _, id := range []int64{7, 9, 99, 0} {
		seedStage2History(t, s, p.principal, serviceapi.Event{ID: fmt.Sprintf("author-%d", id), CreatedAt: at, CreatedBy: id, Type: "lead_added", EntityType: "lead", EntityID: id + 1})
	}
	a := activity.New(presentationSettings{}, p, s, &filteredDirectory{})
	for _, tc := range []struct {
		name    string
		ids     []int64
		unknown bool
		group   int64
		allowed map[int64]bool
	}{
		{"selected", []int64{7}, false, 0, map[int64]bool{7: true}},
		{"selected and unknown", []int64{7}, true, 0, map[int64]bool{7: true, 99: true, 0: true}},
		{"default", nil, false, 0, map[int64]bool{7: true, 9: true, 99: true, 0: true}},
		{"group excludes unknown", nil, true, 1, map[int64]bool{7: true}},
		{"explicit deleted", []int64{99}, false, 0, map[int64]bool{99: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := serviceapi.Query{From: at - 1, To: at + 1, UserIDs: tc.ids, IncludeUnknownAuthors: tc.unknown, GroupID: tc.group, Limit: 1}
			seen := map[int64]bool{}
			for page := 0; page < 5; page++ {
				panel, err := a.Panel(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if panel.Data.Totals.UniqueEvents != int64(len(tc.allowed)) {
					t.Fatalf("totals=%+v", panel.Data.Totals)
				}
				for _, u := range panel.Users {
					if !tc.allowed[u.ID] {
						t.Fatalf("unexpected employee %d", u.ID)
					}
				}
				for _, e := range panel.Data.Events {
					if !tc.allowed[e.CreatedBy] {
						t.Fatalf("unexpected author %d", e.CreatedBy)
					}
					seen[e.CreatedBy] = true
				}
				q.Cursor = panel.Data.NextCursor
				if q.Cursor == "" {
					break
				}
			}
			if len(seen) != len(tc.allowed) {
				t.Fatalf("authors=%v expected=%v", seen, tc.allowed)
			}
		})
	}
}

func TestTimelineUsesAbsoluteBoundariesAcrossDST(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, tz, unit string
		events, starts           []string
	}{
		{"first repeated hour", "2026-11-01T01:00:00-04:00", "2026-11-01T01:59:59-04:00", "America/New_York", serviceapi.BucketHour, []string{"2026-11-01T01:30:00-04:00"}, []string{"2026-11-01T01:00:00-04:00"}},
		{"second repeated hour", "2026-11-01T01:00:00-05:00", "2026-11-01T01:59:59-05:00", "America/New_York", serviceapi.BucketHour, []string{"2026-11-01T01:30:00-05:00"}, []string{"2026-11-01T01:00:00-05:00"}},
		{"both repeated hours", "2026-11-01T01:00:00-04:00", "2026-11-01T01:59:59-05:00", "America/New_York", serviceapi.BucketHour, []string{"2026-11-01T01:30:00-04:00", "2026-11-01T01:30:00-05:00"}, []string{"2026-11-01T01:00:00-04:00", "2026-11-01T01:00:00-05:00"}},
		{"spring skipped hour", "2026-03-08T01:00:00-05:00", "2026-03-08T03:59:59-04:00", "America/New_York", serviceapi.BucketHour, []string{"2026-03-08T01:30:00-05:00", "2026-03-08T03:30:00-04:00"}, []string{"2026-03-08T01:00:00-05:00", "2026-03-08T03:00:00-04:00"}},
		{"25 hour day", "2026-11-01T00:00:00-04:00", "2026-11-01T23:59:59-05:00", "America/New_York", serviceapi.BucketDay, []string{"2026-11-01T23:30:00-05:00"}, []string{"2026-11-01T00:00:00-04:00"}},
		{"fractional offset", "2026-09-01T01:15:00+05:30", "2026-09-01T02:15:00+05:30", "Asia/Kolkata", serviceapi.BucketHour, []string{"2026-09-01T01:30:00+05:30", "2026-09-01T02:00:00+05:30"}, []string{"2026-09-01T01:00:00+05:30", "2026-09-01T02:00:00+05:30"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unix := func(v string) int64 {
				t.Helper()
				at, err := time.Parse(time.RFC3339, v)
				if err != nil {
					t.Fatal(err)
				}
				return at.Unix()
			}
			s, p, _ := setup(t)
			accepted(t, s, p)
			for i, v := range tc.events {
				seedStage2History(t, s, p.principal, serviceapi.Event{ID: fmt.Sprintf("bucket-%d", i), CreatedAt: unix(v), CreatedBy: 7, Type: "lead_added", EntityType: "lead", EntityID: 1})
			}
			got, err := s.Query(context.Background(), serviceapi.Query{From: unix(tc.from), To: unix(tc.to), Limit: 10, Timezone: tc.tz, Buckets: tc.unit})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Timeline) != len(tc.starts) {
				t.Fatalf("timeline=%+v", got.Timeline)
			}
			var n int64
			for i, b := range got.Timeline {
				if b.StartAt != unix(tc.starts[i]) || b.Count != 1 {
					t.Fatalf("bucket %d=%+v", i, b)
				}
				n += b.Count
			}
			if n != got.Totals.UniqueEvents || n != int64(len(tc.events)) {
				t.Fatalf("sum=%d total=%d", n, got.Totals.UniqueEvents)
			}
		})
	}
}
