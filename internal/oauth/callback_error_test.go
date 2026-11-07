package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCallbackFailureOutcomeAndCause(t *testing.T) {
	cause := &pgconn.PgError{Code: "08006", Message: "synthetic database failure"}
	for _, outcome := range []callbackOutcome{callbackRepeatable, callbackConsumed, callbackUncertain} {
		wrapped := fmt.Errorf("callback: %w", withCallbackOutcome(cause, outcome))
		var pgError *pgconn.PgError
		if !errors.Is(wrapped, cause) || !errors.As(wrapped, &pgError) || pgError != cause || callbackOutcomeOf(wrapped) != outcome {
			t.Fatalf("outcome=%d error chain=%v", outcome, wrapped)
		}
	}
	if withCallbackOutcome(nil, callbackConsumed) != nil {
		t.Fatal("successful callback became a failure")
	}
	for _, unclassified := range []error{errors.New("unknown"), errInstallCapacity} {
		if callbackOutcomeOf(unclassified) != callbackUncertain {
			t.Fatalf("unclassified callback failure treated as repeatable: %v", unclassified)
		}
	}
}

func TestCallbackFailureHTTPContract(t *testing.T) {
	for _, query := range []string{
		"state=once&code=code&referer=fixture.amocrm.ru",
		"from_widget=1&client_id=client&code=code&referer=fixture.amocrm.ru",
	} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errInstallCapacity} {
			for _, outcome := range []callbackOutcome{callbackRepeatable, callbackConsumed, callbackUncertain} {
				t.Run(fmt.Sprintf("%s/%v/%d", query, cause, outcome), func(t *testing.T) {
					failure := func(context.Context, string, string, string) (InstallationResult, error) {
						return InstallationResult{}, withCallbackOutcome(cause, outcome)
					}
					h := testHandler(fakeFlow{callback: failure, install: failure}, nil)
					w := requestIDRecorder()
					h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?"+query, nil))
					code, message, requestID, retryable := decodeOAuthError(t, w.Body.Bytes())
					wantStatus, wantCode, wantMessage, wantRetryAfter := http.StatusBadGateway, "unavailable", "authorization failed; restart authorization", ""
					if outcome == callbackRepeatable {
						wantMessage = "authorization failed"
						if cause == errInstallCapacity {
							wantStatus, wantCode, wantRetryAfter = http.StatusTooManyRequests, "rate_limited", "1"
						}
					}
					if w.Code != wantStatus || code != wantCode || message != wantMessage || requestID == "" || retryable != (outcome == callbackRepeatable) || w.Header().Get("Retry-After") != wantRetryAfter || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("outcome=%d status=%d headers=%v body=%s", outcome, w.Code, w.Header(), w.Body)
					}
				})
			}
		}
	}
}
