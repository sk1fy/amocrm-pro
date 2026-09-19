package admincommand

import (
	"testing"
	"time"
)

func TestCheckDelayJitterAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"fixture-a", "fixture-b", "fixture-c"} {
		delay := checkDelay(id, now, time.Hour, 0)
		if delay < 48*time.Minute || delay > 72*time.Minute {
			t.Fatal(delay)
		}
		if checkDelay(id, now, time.Hour, 0) != delay {
			t.Fatal("jitter changed within window")
		}
		if got := checkDelay(id, now, time.Hour, 7200); got != 2*time.Hour {
			t.Fatal(got)
		}
	}
}
func TestSchedulerDefaultsOffAndInvalidConfig(t *testing.T) {
	t.Setenv("ADMIN_CONNECTION_CHECKS_ENABLED", "")
	c, err := LoadCheckConfig()
	if err != nil || c.Enabled || c.Interval != time.Hour {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("ADMIN_CONNECTION_CHECK_INTERVAL", "1h")
	if c, err := LoadCheckConfig(); err != nil || c.Interval != time.Hour {
		t.Fatalf("hourly config: %+v %v", c, err)
	}
	t.Setenv("ADMIN_CONNECTION_CHECK_INTERVAL", "73m")
	if _, err := LoadCheckConfig(); err == nil {
		t.Fatal("unsafe interval accepted")
	}
	t.Setenv("ADMIN_CONNECTION_CHECK_INTERVAL", "1h")
	t.Setenv("ADMIN_CONNECTION_CHECK_CONCURRENCY", "0")
	if _, err := LoadCheckConfig(); err == nil {
		t.Fatal("invalid limit accepted")
	}
}
