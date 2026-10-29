package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// requestTracer records query operations and pool acquisitions into the
// request collector carried by the operation's context. Without a collector
// it returns the context unchanged and records nothing, so background work
// is never attributed to a request (component-persistence, Request database
// metrics and pool sizing).
type requestTracer struct {
	next           pgx.QueryTracer
	nextAcquire    pgxpool.AcquireTracer
	now            func() time.Time
	acquireStarted func(context.Context)
}

var (
	_ pgx.QueryTracer       = (*requestTracer)(nil)
	_ pgxpool.AcquireTracer = (*requestTracer)(nil)
)

// newRequestTracer composes with a tracer already on the parsed
// configuration; pgx.ParseConfig never sets one, so production has none.
func newRequestTracer(next pgx.QueryTracer, now func() time.Time, acquireStarted func(context.Context)) *requestTracer {
	if now == nil {
		now = time.Now
	}
	tracer := &requestTracer{next: next, now: now, acquireStarted: acquireStarted}
	if acquire, ok := next.(pgxpool.AcquireTracer); ok {
		tracer.nextAcquire = acquire
	}
	return tracer
}

type queryStartKey struct{}

type acquireStartKey struct{}

func (t *requestTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t.next != nil {
		ctx = t.next.TraceQueryStart(ctx, conn, data)
	}
	if dbmetrics.FromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, queryStartKey{}, t.now())
}

// TraceQueryEnd runs once per operation: when Exec completes, and when the
// rows of Query or QueryRow close, so the duration includes row consumption.
func (t *requestTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if collector := dbmetrics.FromContext(ctx); collector != nil {
		if started, ok := ctx.Value(queryStartKey{}).(time.Time); ok {
			collector.AddQuery(t.now().Sub(started))
		}
	}
	if t.next != nil {
		t.next.TraceQueryEnd(ctx, conn, data)
	}
}

func (t *requestTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
	if t.nextAcquire != nil {
		ctx = t.nextAcquire.TraceAcquireStart(ctx, pool, data)
	}
	if dbmetrics.FromContext(ctx) == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, acquireStartKey{}, t.now())
	if t.acquireStarted != nil {
		t.acquireStarted(ctx)
	}
	return ctx
}

// TraceAcquireEnd records every acquisition, including a failed or cancelled
// one. The latency includes pool management overhead, not only queueing.
func (t *requestTracer) TraceAcquireEnd(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if collector := dbmetrics.FromContext(ctx); collector != nil {
		if started, ok := ctx.Value(acquireStartKey{}).(time.Time); ok {
			collector.AddAcquire(t.now().Sub(started))
		}
	}
	if t.nextAcquire != nil {
		t.nextAcquire.TraceAcquireEnd(ctx, pool, data)
	}
}

// RequestMetrics reports that PostgreSQL measures all three request fields.
func (s *Store) RequestMetrics() dbmetrics.Capability {
	return dbmetrics.QueryCount | dbmetrics.QueryDuration | dbmetrics.AcquireDuration
}

// PoolStats is the process-wide pool snapshot the daemon logs.
func (s *Store) PoolStats() dbmetrics.PoolStats {
	return poolStatsFrom(s.pool.Stat())
}

func poolStatsFrom(stat *pgxpool.Stat) dbmetrics.PoolStats {
	return dbmetrics.PoolStats{
		AcquiredConns:     stat.AcquiredConns(),
		IdleConns:         stat.IdleConns(),
		MaxConns:          stat.MaxConns(),
		EmptyAcquireCount: stat.EmptyAcquireCount(),
		AcquireDuration:   stat.AcquireDuration(),
	}
}
