package requesttiming

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

type existingTracer struct{ starts, ends int }

func (t *existingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.starts++
	return ctx
}
func (t *existingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) { t.ends++ }
func TestDisabledPostgresTracingPreservesContextAndExistingObserver(t *testing.T) {
	ctx := context.Background()
	old := &existingTracer{}
	tracer := PGXTracer{Query: old}
	got := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
	tracer.TraceQueryEnd(got, nil, pgx.TraceQueryEndData{})
	if got != ctx || old.starts != 1 || old.ends != 1 {
		t.Fatal("disabled trace replaced context or observer")
	}
	if tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{}) != ctx {
		t.Fatal("disabled acquire replaced context")
	}
}
