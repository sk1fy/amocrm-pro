package activitybridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestDeliveryDrainsReadyWorkAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	b := New(nil, nil, nil, nil)
	calls := 0
	if err := b.runDelivery(ctx, func(context.Context) (bool, error) {
		calls++
		if calls == 4 {
			cancel()
			return false, nil
		}
		return true, nil
	}); err != nil || calls != 4 {
		t.Fatalf("ready work waited for polling tick: calls=%d err=%v", calls, err)
	}
}

func TestDeliveryFailureBacksOffAndLogsNoRawError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	b := New(nil, nil, nil, nil)
	b.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	calls := 0
	_ = b.runDelivery(ctx, func(context.Context) (bool, error) {
		calls++
		return true, errors.New("private DSN and credentials")
	})
	if calls != 1 || !strings.Contains(logs.String(), `"code":"internal"`) || strings.Contains(logs.String(), "private") {
		t.Fatalf("failure backoff/log sanitization: calls=%d log=%s", calls, logs.String())
	}
}

type capabilityRow struct{ err error }

func (r capabilityRow) Scan(...any) error { return r.err }

type capabilityQuery struct{ err error }

func (q capabilityQuery) QueryRow(context.Context, string, ...any) pgx.Row {
	return capabilityRow{q.err}
}

func TestCapabilityStorageFailureIsNotReportedAsRevocation(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"disabled", pgx.ErrNoRows, 403},
		{"database_down", errors.New("secret database details"), 503},
		{"database_timeout", context.DeadlineExceeded, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireActivityEnabled(context.Background(), capabilityQuery{test.err}, uuid.New())
			w := httptest.NewRecorder()
			writeError(w, err)
			if w.Code != test.status || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
		})
	}
}
