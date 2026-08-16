package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func TestRequirementEventLookupIndexIntegration(t *testing.T) {
	st, _, _ := newPhase61IntegrationStore(t)
	defer st.Close()
	var definition string
	if err := st.pool.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes
		WHERE schemaname=current_schema() AND indexname='events_requirement_document_idx'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"workspace_id", "payload_json ->> 'requirement_id'", "at", "id", "task_id IS NULL"} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("requirement event index %q missing %q", definition, fragment)
		}
	}
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
	links, err := st.ListLineageNeighborhood(ctx, roots, core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes, Workspace: workspace})
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

func TestTaskOperationsProjectionPaginatesAndBatchesPageDataIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	// Task IDs are globally unique, so every fixture ID carries a run suffix.
	suffix := core.NewTaskID()
	wanted := "ops-running-2-" + suffix
	for index, task := range []core.Task{
		phase61Task(workspace, "ops-queued-"+suffix, core.TaskQueued, ""),
		phase61Task(workspace, "ops-running-1-"+suffix, core.TaskRunning, ""),
		phase61Task(workspace, wanted, core.TaskRunning, ""),
	} {
		task.CreatedAt = time.Date(2026, 8, 7, 10, index, 0, 0, time.UTC)
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: wanted, Content: "## Approach\n\n## Done criteria\n",
		Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: wanted, Kind: store.TaskContextRequirementAdded,
		Payload: core.JSONPayload(map[string]any{"id": "req-ops", "version": 1})}); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListTaskOperations(ctx, store.TaskOperationsQuery{
		TaskFilter: store.TaskFilter{
			States: []core.TaskState{core.TaskRunning}, Repositories: []string{"conveyor"},
		}, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Tasks) != 1 || page.Tasks[0].ID != wanted {
		t.Fatalf("page = total:%d tasks:%+v", page.Total, page.Tasks)
	}
	if page.Plans[wanted].Version != 1 || len(page.Events[wanted]) != 1 ||
		page.Events[wanted][0].Kind != store.TaskContextRequirementAdded {
		t.Fatalf("batched page data = plans:%+v events:%+v", page.Plans, page.Events)
	}
}

func TestTaskOperationsPaginationBoundsMatchTheMemoryStoreIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	created := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	ids := map[string]string{
		"oldest": "bounds-oldest-" + suffix,
		"zeta":   "bounds-zeta-" + suffix,
		"alpha":  "bounds-alpha-" + suffix,
		"newest": "bounds-newest-" + suffix,
	}
	for _, fixture := range []struct {
		id string
		at time.Time
	}{
		{id: ids["oldest"], at: created.Add(-time.Hour)},
		{id: ids["zeta"], at: created},
		{id: ids["alpha"], at: created},
		{id: ids["newest"], at: created.Add(time.Hour)},
	} {
		task := phase61Task(workspace, fixture.id, core.TaskQueued, "")
		task.CreatedAt = fixture.at
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	storetest.RunTaskOperationsPaginationConformance(t, storetest.TaskOperationsFixture{
		Store: st, Context: ctx, WantTotal: 4,
		WantOrder: []string{ids["newest"], ids["alpha"], ids["zeta"], ids["oldest"]},
		Filter:    store.TaskFilter{Repositories: []string{"conveyor"}},
	})
}

func TestCallerAttentionFiltersBeforePagingIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	userID := "usr_attention_" + core.NewTaskID()
	if _, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: userID, Email: userID + "@example.test", DisplayName: "Attention Assignee",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'contributor')`, workspace, userID); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	oldID := "attention-old-" + core.NewTaskID()
	old := phase61Task(workspace, oldID, core.TaskAwaiting, "")
	old.CreatedAt = created
	if err := st.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, oldID, userID); err != nil {
		t.Fatal(err)
	}
	otherUserID := "usr_attention_other_" + core.NewTaskID()
	if _, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: otherUserID, Email: otherUserID + "@example.test", DisplayName: "Other Assignee",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'contributor')`, workspace, otherUserID); err != nil {
		t.Fatal(err)
	}
	otherUserTask := phase61Task(workspace, "attention-other-user-"+core.NewTaskID(), core.TaskAwaiting, "")
	if err := st.CreateTask(ctx, otherUserTask); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, otherUserTask.ID, otherUserID); err != nil {
		t.Fatal(err)
	}
	anchor := phase61Task(workspace, "attention-anchor-"+core.NewTaskID(), core.TaskAwaiting, "")
	if err := st.CreateTask(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, anchor.ID, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, phase61Task(workspace, "attention-child-"+core.NewTaskID(), core.TaskQueued, anchor.ID)); err != nil {
		t.Fatal(err)
	}
	otherWorkspace := "attention-other-workspace-" + core.NewTaskID()
	otherContext := store.WithWorkspace(context.Background(), otherWorkspace)
	if _, err := st.BootstrapWorkspaceConfig(otherContext, &config.Config{
		Workspace: otherWorkspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	otherWorkspaceTask := phase61Task(otherWorkspace, "attention-other-workspace-task-"+core.NewTaskID(), core.TaskAwaiting, "")
	if err := st.CreateTask(otherContext, otherWorkspaceTask); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(otherContext, otherWorkspaceTask.ID, userID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 125; index++ {
		id := fmt.Sprintf("attention-new-%03d-%s", index, core.NewTaskID())
		task := phase61Task(workspace, id, core.TaskQueued, "")
		task.CreatedAt = created.Add(time.Duration(index+1) * time.Minute)
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(st).SetAssignee(ctx, id, userID); err != nil {
			t.Fatal(err)
		}
	}

	page, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: userID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].ID != oldID {
		t.Fatalf("attention page = total:%d tasks:%+v", page.Total, page.Tasks)
	}
}

// The shared filter is SQL here and Go in the memory store, so both run the
// same cases (AC-2.4).
func TestTaskFilterMatchesTheMemoryStoreIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	assigneeID := "usr_filter_" + core.NewTaskID()
	user, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: assigneeID, Email: assigneeID + "@example.test", DisplayName: "Filter Assignee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'contributor')`, workspace, user.ID); err != nil {
		t.Fatal(err)
	}
	fixture := storetest.TaskFilterFixture{
		Store: st, Context: ctx, Workspace: workspace, Repo: "conveyor", Suffix: core.NewTaskID(),
		AssigneeUserID: assigneeID,
		Assign: func(t *testing.T, taskID, userID string) {
			t.Helper()
			if _, err = taskops.New(st).SetAssignee(ctx, taskID, userID); err != nil {
				t.Fatal(err)
			}
		},
	}
	storetest.SeedTaskFilterFixture(t, fixture)
	storetest.RunTaskFilterConformance(t, fixture)
}

func TestCheckpointContextCandidatesMatchDirectActiveContextIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	now := time.Now().UTC()
	seed := func(prefix, reason string) string {
		t.Helper()
		id := prefix + "-" + suffix
		task := phase61Task(workspace, id, core.TaskRunning, "")
		task.Title, task.CreatedAt = prefix, now
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
			ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued,
			LastAttemptOutcome: core.WorkOrderOutcomeReleased, LastFailureMessage: reason,
			QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	eligible := seed("eligible", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	seed("other-release", "worker stopped")
	attached := seed("attached", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	if err := st.AppendEvent(ctx, core.Event{TaskID: attached, Kind: store.TaskContextRequirementAdded,
		Payload: core.JSONPayload(map[string]any{"id": "req-confirmed"})}); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListCheckpointContextCandidates(ctx, "req-confirmed")
	if err != nil || len(got) != 1 || got[0].ID != eligible {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
}

// A Tasks page pays for its page. The activity-marker projection used to read
// every work order in the workspace and discard the rest, which is exactly the
// unbounded read the Tasks list was rewritten to avoid.
func TestActivityMarkersForTasksScopeEveryReadToThePageIntegration(t *testing.T) {
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
	workspace := "activity-scope-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	suffix := core.NewTaskID()
	paged := phase61Task(workspace, "scope-paged-"+suffix, core.TaskRunning, "")
	offPage := phase61Task(workspace, "scope-off-page-"+suffix, core.TaskRunning, "")
	for _, task := range []core.Task{paged, offPage} {
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: task.ID + "-job", TaskID: task.ID, Stage: core.StageImplement, StartedAt: time.Now().UTC()}
		if err = st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
			ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement,
			State: core.WorkOrderQueued, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	recorder.reset()
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{paged.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 || markers[0].TaskID != paged.ID {
		t.Fatalf("markers = %+v", markers)
	}
	for _, query := range recorder.snapshot() {
		if !strings.Contains(query, "work_orders") || strings.Contains(query, "EXISTS (SELECT 1 FROM work_orders") {
			continue
		}
		if !strings.Contains(query, "task_id=ANY") && !strings.Contains(query, "task_id=$") {
			t.Fatalf("Tasks page scanned workspace-wide work-order data: %s", query)
		}
	}

	// The unscoped activity feed keeps its workspace-wide read.
	recorder.reset()
	if _, err = st.ListActivityMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	workspaceWide := false
	for _, query := range recorder.snapshot() {
		workspaceWide = workspaceWide || (strings.Contains(query, "FROM work_orders") && !strings.Contains(query, "task_id=ANY"))
	}
	if !workspaceWide {
		t.Fatal("activity feed lost its workspace-wide work-order read")
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
