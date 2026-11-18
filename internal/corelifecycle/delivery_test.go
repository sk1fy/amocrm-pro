package corelifecycle

import (
	"net/http"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

func TestDeliveryRetryAfter(t *testing.T) {
	now := time.Date(2026, time.November, 17, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status int
		value  string
		delay  time.Duration
	}{
		{http.StatusTooManyRequests, "19", 19 * time.Second},
		{http.StatusServiceUnavailable, now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{http.StatusTooManyRequests, "18446744073709551616", 10 * time.Minute},
	} {
		header := http.Header{}
		header.Set("Retry-After", test.value)
		response := &http.Response{StatusCode: test.status, Header: header}
		failure := jobs.Classify(deliveryResponseError(response, now), 1)
		if !failure.Retryable || failure.RetryAfter != test.delay {
			t.Fatalf("Retry-After %q yielded %+v", test.value, failure)
		}
	}
}
