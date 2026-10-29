package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// stepClock advances by step on every reading.
type stepClock struct {
	current time.Time
	step    time.Duration
}

func (c *stepClock) now() time.Time {
	c.current = c.current.Add(c.step)
	return c.current
}

func TestRequestTracerRecordsQueriesAndAcquires(t *testing.T) {
	clock := &stepClock{current: time.Unix(0, 0), step: 2 * time.Millisecond}
	tracer := newRequestTracer(nil, clock.now, nil)
	collector := dbmetrics.NewCollector()
	ctx := dbmetrics.WithCollector(context.Background(), collector)

	// A completed query, a failed query, and a query whose rows closed
	// after consumption each end once.
	for _, end := range []pgx.TraceQueryEndData{{}, {Err: errors.New("relation missing")}, {}} {
		queryCtx := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
		tracer.TraceQueryEnd(queryCtx, nil, end)
	}
	// A successful, a failed, and a cancelled acquisition.
	for _, end := range []pgxpool.TraceAcquireEndData{{}, {Err: errors.New("dial failed")}, {Err: context.Canceled}} {
		acquireCtx := tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
		tracer.TraceAcquireEnd(acquireCtx, nil, end)
	}
	want := dbmetrics.Snapshot{Queries: 3, QueryDuration: 6 * time.Millisecond, AcquireDuration: 6 * time.Millisecond}
	if got := collector.Snapshot(); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestRequestTracerWithoutCollectorIsNoOp(t *testing.T) {
	readings := 0
	tracer := newRequestTracer(nil, func() time.Time { readings++; return time.Now() }, func(context.Context) { t.Fatal("hook must not run without a collector") })
	ctx := context.Background()
	if got := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}); got != ctx {
		t.Fatal("query start must return the same context")
	}
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	if got := tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{}); got != ctx {
		t.Fatal("acquire start must return the same context")
	}
	tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{})
	if readings != 0 {
		t.Fatalf("clock read %d times without a collector", readings)
	}
}

func TestRequestTracerCallsAcquireHookWithCollectorContext(t *testing.T) {
	collector := dbmetrics.NewCollector()
	var seen *dbmetrics.Collector
	tracer := newRequestTracer(nil, time.Now, func(ctx context.Context) { seen = dbmetrics.FromContext(ctx) })
	tracer.TraceAcquireStart(dbmetrics.WithCollector(context.Background(), collector), nil, pgxpool.TraceAcquireStartData{})
	if seen != collector {
		t.Fatal("acquire hook did not receive the request collector")
	}
}

type recordingTracer struct{ calls []string }

type recordingKey struct{}

func (r *recordingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	r.calls = append(r.calls, "query-start")
	return context.WithValue(ctx, recordingKey{}, "query")
}

func (r *recordingTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	r.calls = append(r.calls, fmt.Sprintf("query-end:%v", ctx.Value(recordingKey{})))
}

func (r *recordingTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	r.calls = append(r.calls, "acquire-start")
	return context.WithValue(ctx, recordingKey{}, "acquire")
}

func (r *recordingTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	r.calls = append(r.calls, fmt.Sprintf("acquire-end:%v", ctx.Value(recordingKey{})))
}

func TestRequestTracerForwardsToExistingTracer(t *testing.T) {
	existing := &recordingTracer{}
	clock := &stepClock{current: time.Unix(0, 0), step: time.Millisecond}
	tracer := newRequestTracer(existing, clock.now, nil)
	collector := dbmetrics.NewCollector()
	ctx := dbmetrics.WithCollector(context.Background(), collector)
	tracer.TraceQueryEnd(tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}), nil, pgx.TraceQueryEndData{})
	tracer.TraceAcquireEnd(tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{}), nil, pgxpool.TraceAcquireEndData{})
	want := []string{"query-start", "query-end:query", "acquire-start", "acquire-end:acquire"}
	if fmt.Sprint(existing.calls) != fmt.Sprint(want) {
		t.Fatalf("forwarded calls = %v, want %v", existing.calls, want)
	}
	if got := collector.Snapshot(); got.Queries != 1 || got.QueryDuration != time.Millisecond || got.AcquireDuration != time.Millisecond {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestRequestTracerKeepsNoSQLOrArguments(t *testing.T) {
	const sentinel = "sentinel-workspace-7f3a"
	tracer := newRequestTracer(nil, time.Now, nil)
	collector := dbmetrics.NewCollector()
	ctx := dbmetrics.WithCollector(context.Background(), collector)
	queryCtx := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT * FROM tasks WHERE workspace_id='" + sentinel + "'", Args: []any{sentinel}})
	tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: errors.New(sentinel)})
	if rendered := fmt.Sprintf("%+v %#v", collector.Snapshot(), collector.Snapshot()); strings.Contains(rendered, sentinel) {
		t.Fatalf("snapshot carries request data: %s", rendered)
	}
}

func TestPoolStatsMapsUnits(t *testing.T) {
	options, err := resolveOpenOptions([]OpenOption{WithMaxConns(9)})
	if err != nil {
		t.Fatal(err)
	}
	config, err := poolConfig("postgres://u:secret@192.0.2.1:5432/conveyor", options)
	if err != nil {
		t.Fatal(err)
	}
	// MinConns is zero, so creating the pool opens no connection.
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := &Store{pool: pool}
	got := st.PoolStats()
	if got != (dbmetrics.PoolStats{MaxConns: 9}) {
		t.Fatalf("pool stats = %+v", got)
	}
	if capability := st.RequestMetrics(); capability != dbmetrics.QueryCount|dbmetrics.QueryDuration|dbmetrics.AcquireDuration {
		t.Fatalf("capability = %b", capability)
	}
}
