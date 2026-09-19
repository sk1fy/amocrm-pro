package connectioncheck

import (
	"testing"
	"time"
)

func TestFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kind string
		age  time.Duration
		want string
	}{{"verified_ok", 80 * time.Minute, "fresh"}, {"verified_ok", 90 * time.Minute, "fresh"}, {"verified_ok", 91 * time.Minute, "stale"}, {"verified_ok", FreshFor, "fresh"}, {"verified_ok", FreshFor + 1, "stale"}, {"network_error", 0, "unavailable"}, {"auth_error", 0, "fresh"}, {"future", 0, "unknown"}} {
		at := now.Add(-tc.age)
		got := New(tc.kind, &at, 0, now)
		if got.Freshness != tc.want || got.FreshForSeconds != 5400 {
			t.Fatalf("%+v: %+v", tc, got)
		}
	}
}
