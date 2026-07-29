package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.queries = append(r.queries, data.SQL)
	r.mu.Unlock()
	return ctx
}

func (*queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *queryRecorder) reset() {
	r.mu.Lock()
	r.queries = nil
	r.mu.Unlock()
}

func (r *queryRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func TestWorkOrderReadsExecuteNoMutationQueriesIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	st := &Store{pool: pool, queries: db.New(pool)}
	ctx := store.WithWorkspace(t.Context(), "pure-read-"+strings.ToLower(t.Name()))
	recorder.reset()

	_, _ = st.GetWorkOrder(ctx, "missing")
	if _, err = st.ListWorkOrders(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ListTaskWorkOrders(ctx, "missing"); err != nil {
		t.Fatal(err)
	}

	queries := recorder.snapshot()
	if len(queries) < 3 {
		t.Fatalf("captured queries=%v", queries)
	}
	for _, query := range queries {
		upper := strings.ToUpper(strings.TrimSpace(query))
		if strings.HasPrefix(upper, "UPDATE ") || strings.HasPrefix(upper, "INSERT ") || strings.HasPrefix(upper, "DELETE ") {
			t.Fatalf("observational read executed mutation query: %s", query)
		}
	}
}
