// Package connectioncheck defines the safe, server-computed verification fact.
package connectioncheck

import "time"

// FreshFor is shared by reads, filters and monitoring. The scheduler target is
// one hour with twenty percent jitter, below this ninety-minute window.
const FreshFor = 90 * time.Minute

type Snapshot struct {
	Classification  string     `json:"classification"`
	ObservedAt      *time.Time `json:"observed_at"`
	Freshness       string     `json:"freshness"`
	FreshForSeconds int64      `json:"fresh_for_seconds"`
	RetryAfter      int64      `json:"retry_after,omitempty"`
	Error           *Error     `json:"error,omitempty"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func New(classification string, observed *time.Time, retry int64, now time.Time) Snapshot {
	s := Snapshot{Classification: classification, ObservedAt: observed, RetryAfter: retry, FreshForSeconds: int64(FreshFor.Seconds()), Freshness: "fresh"}
	switch classification {
	case "verified_ok", "auth_error":
	case "network_error", "rate_limited", "internal_error":
		s.Freshness = "unavailable"
		s.Error = &Error{Code: classification, Message: "The latest amoCRM check could not confirm access"}
	default:
		s.Classification, s.Freshness, s.ObservedAt = "unknown", "unknown", nil
		return s
	}
	if observed == nil {
		s.Classification, s.Freshness = "unknown", "unknown"
		return s
	}
	if now.Sub(*observed) > FreshFor {
		s.Freshness = "stale"
	}
	return s
}
