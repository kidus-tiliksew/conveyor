package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func TestLineageNeighborhoodBatchesManyRootsInOneScopedQueryIntegration(t *testing.T) {
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
	workspace := "lineage-query-" + strings.ToLower(t.Name())
	if _, err = pool.Exec(t.Context(), `INSERT INTO workspaces (id,name) VALUES ($1,$1) ON CONFLICT DO NOTHING`, workspace); err != nil {
		t.Fatal(err)
	}
	roots := make([]core.LineageNode, 0, 50)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("req-%02d", i)
		roots = append(roots, core.LineageNode{Type: core.LineageRequirement, ID: id})
		if _, err = pool.Exec(t.Context(), `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event) VALUES ($1,'requirement',$2,'task',$3,'historical_feature_assignment','feature.migrated')`, workspace, id, "task-"+id); err != nil {
			t.Fatal(err)
		}
	}
	recorder.reset()
	ctx := store.WithWorkspace(t.Context(), workspace)
	links, err := st.ListLineageNeighborhood(ctx, roots, core.LineageTraversalBudget{MaxDepth: core.ContextLineageMaxDepth, MaxNodes: core.ContextLineageMaxNodes, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 50 {
		t.Fatalf("links=%d want 50", len(links))
	}
	if _, err = st.ListArtifactsForLineage(ctx, roots); err != nil {
		t.Fatal(err)
	}
	queries := recorder.snapshot()
	if len(queries) != 2 {
		t.Fatalf("scoped query count=%d want 2: %v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "WITH RECURSIVE seeds") || !strings.Contains(queries[1], "WITH wanted") {
		t.Fatalf("unexpected scoped queries: %v", queries)
	}
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
