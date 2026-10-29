package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// detailReadTracer records each query's SQL and returned row count at the
// driver boundary (component-persistence, Task-detail batch reads).
type detailReadTracer struct {
	mu        sync.Mutex
	recording bool
	queries   []string
	rows      int64
}

type detailReadTracerKey struct{}

func (r *detailReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, detailReadTracerKey{}, data.SQL)
}

func (r *detailReadTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(detailReadTracerKey{}).(string)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recording {
		r.queries = append(r.queries, sql)
		r.rows += data.CommandTag.RowsAffected()
	}
}

func (r *detailReadTracer) observe(t *testing.T, read func() error) ([]string, int64) {
	t.Helper()
	r.mu.Lock()
	r.recording, r.queries, r.rows = true, nil, 0
	r.mu.Unlock()
	if err := read(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	return r.queries, r.rows
}

// TestTaskDetailReadsIssueOneBoundedQueryIntegration observes the three
// detail selections before and after unrelated captures, document versions
// and bodies, other documents, and artifact blobs grow: each selection stays
// one query with the same returned rows and values, and no selected query
// reads a version, artifact, or blob table.
func TestTaskDetailReadsIssueOneBoundedQueryIntegration(t *testing.T) {
	const sentinel = "POSTGRES-DETAIL-READ-SENTINEL"
	cfg, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	tracer := &detailReadTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "detail-reads-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	seed := storetest.SeedTaskDetailReads(t, st, ctx, "pg-detail-"+strings.ToLower(core.NewTaskID()))
	type observation struct {
		queries int
		rows    int64
	}
	observe := func() (map[string]observation, string) {
		t.Helper()
		out := map[string]observation{}
		for name, read := range map[string]func() error{
			"captures":  func() error { _, err := st.ListTaskTranscriptCaptures(ctx, seed.TaskID); return err },
			"snapshots": func() error { _, err := st.ListTaskActivitySnapshots(ctx, seed.TaskID, seed.OrderIDs); return err },
			"metadata":  func() error { _, err := st.ListDocumentMetadata(ctx, seed.DocumentIDs); return err },
		} {
			queries, rows := tracer.observe(t, read)
			for _, query := range queries {
				if strings.Contains(query, "_versions") || strings.Contains(query, "artifact") || strings.Contains(query, "events") {
					t.Fatalf("%s selected unrelated history: %s", name, query)
				}
			}
			out[name] = observation{queries: len(queries), rows: rows}
		}
		return out, storetest.ReadTaskDetailSelections(t, st, ctx, seed)
	}
	before, beforeValues := observe()
	want := map[string]observation{"captures": {1, 2}, "snapshots": {1, 1}, "metadata": {1, 2}}
	if len(before) != len(want) || before["captures"] != want["captures"] || before["snapshots"] != want["snapshots"] || before["metadata"] != want["metadata"] {
		t.Fatalf("selections = %+v, want %+v", before, want)
	}
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 2)
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 8)
	after, afterValues := observe()
	if after["captures"] != before["captures"] || after["snapshots"] != before["snapshots"] || after["metadata"] != before["metadata"] {
		t.Fatalf("unrelated growth changed the selections: before %+v after %+v", before, after)
	}
	if afterValues != beforeValues || strings.Contains(afterValues, sentinel) {
		t.Fatalf("unrelated growth changed returned values:\nbefore %s\nafter  %s", beforeValues, afterValues)
	}
}
