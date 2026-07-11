package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

type errorQueryer struct{ err error }

func (q errorQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	return errorRow{err: q.err}
}

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

func TestBacklogCollectorValidationAndCollectionFailure(t *testing.T) {
	if _, err := NewBacklogCollector(nil, time.Second); err == nil {
		t.Fatal("expected nil queryer rejection")
	}
	if _, err := NewBacklogCollector(errorQueryer{}, 0); err == nil {
		t.Fatal("expected timeout rejection")
	}

	collector, err := NewBacklogCollector(errorQueryer{err: errors.New("database unavailable")}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	if _, err := registry.Gather(); err == nil {
		t.Fatal("expected collection failure to surface as scrape error")
	}
}
