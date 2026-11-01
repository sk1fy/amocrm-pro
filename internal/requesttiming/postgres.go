package requesttiming

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGXTracer composes the existing query observer and adds pool acquisition.
type PGXTracer struct{ Query pgx.QueryTracer }
type queryKey struct{}
type acquireKey struct{}

func (t PGXTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if t.Query != nil {
		ctx = t.Query.TraceQueryStart(ctx, conn, d)
	}
	if !Enabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, queryKey{}, Measure(ctx, SQL))
}
func (t PGXTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryEndData) {
	if f, ok := ctx.Value(queryKey{}).(func(error)); ok {
		f(d.Err)
	}
	if t.Query != nil {
		t.Query.TraceQueryEnd(ctx, conn, d)
	}
}
func (t PGXTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if !Enabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, acquireKey{}, Measure(ctx, Acquire))
}
func (t PGXTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	if f, ok := ctx.Value(acquireKey{}).(func(error)); ok {
		f(d.Err)
	}
}
