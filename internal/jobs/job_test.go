package jobs

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyPermanentError(t *testing.T) {
	failure := Classify(Permanent("invalid_payload", errors.New("bad input")), 1)
	if failure.Retryable {
		t.Fatal("permanent error must not be retryable")
	}
	if failure.Code != "invalid_payload" {
		t.Fatalf("unexpected code: %s", failure.Code)
	}
	if failure.RetryAfter != 0 {
		t.Fatalf("permanent error has retry delay: %s", failure.RetryAfter)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	maximum := 10 * time.Minute
	for attempt := 1; attempt < 100; attempt++ {
		delay := Backoff(attempt, 5*time.Second, maximum)
		if delay < 5*time.Second || delay > maximum {
			t.Fatalf("attempt %d: delay outside bounds: %s", attempt, delay)
		}
	}
}
