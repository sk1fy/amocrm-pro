package productplatform

import (
	"testing"
	"time"
)

func TestInitialTermCalendarAndReadDeadline(t *testing.T) {
	const stamp = time.RFC3339Nano
	installed, err := time.Parse(stamp, "2024-02-29T04:05:06.123456789+03:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		rule, until, source string
	}{
		{"free", "2034-02-28T01:05:06.123456789Z", "free"},
		{"top_90d", "2024-05-29T01:05:06.123456789Z", "own_trial"},
		{"pro_7d", "2024-03-07T01:05:06.123456789Z", "own_trial"},
		{"pro_14d", "2024-03-14T01:05:06.123456789Z", "own_trial"},
	} {
		t.Run(test.rule, func(t *testing.T) {
			until, source, err := (initialTermPolicy{rule: test.rule}).term(installed)
			if err != nil || until.Format(stamp) != test.until || source != test.source {
				t.Fatalf("unexpected term: %s (%q), %v", until, source, err)
			}
			row := accessProjection{until: &until, source: &source, version: 1}
			if !row.state(until.Add(-time.Nanosecond)).Active || row.state(until).Active {
				t.Fatal("access did not end at the exact term deadline")
			}
		})
	}
}
