package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
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

// TestTaskDetailReadsIssueOneBoundedQueryIntegration observes the detail
// metadata selection before and after unrelated orders, document versions
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
			"metadata": func() error { _, err := st.ListDocumentMetadata(ctx, seed.DocumentIDs); return err },
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
	want := map[string]observation{"metadata": {1, 2}}
	if len(before) != len(want) || before["metadata"] != want["metadata"] {
		t.Fatalf("selections = %+v, want %+v", before, want)
	}
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 2)
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 8)
	after, afterValues := observe()
	if after["metadata"] != before["metadata"] {
		t.Fatalf("unrelated growth changed the selections: before %+v after %+v", before, after)
	}
	if afterValues != beforeValues || strings.Contains(afterValues, sentinel) {
		t.Fatalf("unrelated growth changed returned values:\nbefore %s\nafter  %s", beforeValues, afterValues)
	}
	// Growing the selected output keeps one query and one metadata row per
	// requested tier and ID.
	storetest.GrowSelectedTaskDetail(t, st, ctx, &seed, 4)
	grown, _ := observe()
	if grown["metadata"] != (observation{1, before["metadata"].rows + 4}) {
		t.Fatalf("selected growth = %+v (before %+v)", grown, before)
	}
}

// TestTaskDetailSelectsNoPinBodyIntegration serves the real task detail
// route over PostgreSQL and records every statement it issues: none selects
// a served-requirement snapshot or governance snapshot content, and the
// orders that hold a pin are still marked audit_available
// (component-web-task-surfaces invariant 22).
func TestTaskDetailSelectsNoPinBodyIntegration(t *testing.T) {
	const sentinel = "POSTGRES-DETAIL-PIN-BODY-SENTINEL"
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
	workspace := "detail-pins-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Title: "pinned " + id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	task.SetupContract.VerifyStage = true
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	served := []core.ServedRequirementContext{{ID: "req-detail-pins", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: sentinel}}}}
	governance := &core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-detail-pins", Title: "Detail pins", Category: "component", Version: 1, Content: strings.Repeat(sentinel, 64), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
	// The first order is claimed with both pins and released, which keeps
	// them; the second is claimed with neither and stays live.
	pinned, plain := task.ID+"-verify-1", task.ID+"-verify-2"
	for i, orderID := range []string{pinned, plain} {
		if err = st.CreateJob(ctx, core.Job{ID: orderID, TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		at := task.CreatedAt.Add(time.Duration(i+1)*time.Second - time.Minute)
		if err = storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: task.ID, JobID: orderID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: at, QueueDeadline: at.Add(time.Hour), CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		claim := core.WorkOrderClaim{ClaimantID: "detail-pins-worker", SessionID: "pins-" + orderID, ClientToken: "pins-" + orderID, Lease: time.Hour, ExecutionTimeout: time.Hour}
		if orderID == pinned {
			claim.Requirements, claim.Governance = served, governance
		}
		claimed, claimErr := storetest.ClaimWorkOrder(ctx, st, orderID, claim)
		if claimErr != nil {
			t.Fatalf("claim %s: %v", orderID, claimErr)
		}
		if orderID == pinned {
			if _, err = storetest.ReleaseWorkerClaim(ctx, st, orderID, claim.ClaimantID, core.WorkOrderRelease{SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond}); err != nil {
				t.Fatalf("release %s: %v", orderID, err)
			}
		}
	}
	// The shared operator token stands in for a signed-in operator, so the
	// route reads PostgreSQL with no identity registry; only authentication
	// differs from production, not a store read.
	server := httpapi.NewServer(st)
	server.Credentials, server.Memberships = nil, nil
	server.BearerToken, server.Workspace = "operator-token", workspace
	for _, view := range []string{"", "&view=bounded"} {
		var response *httptest.ResponseRecorder
		queries, _ := tracer.observe(t, func() error {
			request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id="+workspace+view, nil)
			request.Header.Set("Authorization", "Bearer operator-token")
			response = httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			return nil
		})
		if response.Code != http.StatusOK {
			t.Fatalf("detail%s: status %d %s", view, response.Code, response.Body)
		}
		if len(queries) == 0 {
			t.Fatalf("detail%s recorded no statement", view)
		}
		orderReads := 0
		for _, query := range queries {
			if strings.Contains(query, "gs.content") || strings.Contains(query, "governance_snapshots gs WHERE") {
				t.Fatalf("detail%s selected governance snapshot content: %s", view, query)
			}
			if !strings.Contains(query, "FROM work_orders") {
				continue
			}
			orderReads++
			// The pin read names the served column only to measure it.
			measured := strings.ReplaceAll(query, "jsonb_array_length(w.served_requirement_snapshot)", "")
			if strings.Contains(measured, "served_requirement_snapshot") || strings.Contains(query, "AS governance_snapshot") {
				t.Fatalf("detail%s selected a pin body: %s", view, query)
			}
		}
		if orderReads == 0 {
			t.Fatalf("detail%s recorded no order read", view)
		}
		body := response.Body.String()
		if strings.Contains(body, sentinel) {
			t.Fatalf("detail%s response carried a pin body", view)
		}
		var decoded struct {
			WorkOrders []struct {
				ID             string `json:"id"`
				AuditAvailable bool   `json:"audit_available"`
			} `json:"work_orders"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		marks := map[string]bool{}
		for _, order := range decoded.WorkOrders {
			marks[order.ID] = order.AuditAvailable
		}
		if len(marks) != 2 || !marks[pinned] || marks[plain] {
			t.Fatalf("detail%s audit marks = %v, want only %s", view, marks, pinned)
		}
	}
}
