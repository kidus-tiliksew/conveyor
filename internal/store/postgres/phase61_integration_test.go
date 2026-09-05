package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type phase61QueryTracer struct {
	mu                  sync.Mutex
	dependencyBatches   int
	childBatches        int
	blockerBatchQueries int
	lifecycleBatches    int
}

func (tracer *phase61QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if strings.Contains(data.SQL, "edge.task_id=ANY") {
		if strings.Contains(data.SQL, "dependency.title") {
			tracer.dependencyBatches++
		} else if strings.Contains(data.SQL, "dependency.state<>'merged'") {
			tracer.blockerBatchQueries++
		}
	}
	if strings.Contains(data.SQL, "parent_task_id=ANY") {
		tracer.childBatches++
	}
	if strings.Contains(data.SQL, "FROM github_lifecycles") && strings.Contains(data.SQL, "task_id=ANY") {
		tracer.lifecycleBatches++
	}
	return ctx
}

func (*phase61QueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tracer *phase61QueryTracer) reset() {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	tracer.dependencyBatches = 0
	tracer.childBatches = 0
	tracer.blockerBatchQueries = 0
	tracer.lifecycleBatches = 0
}

func (tracer *phase61QueryTracer) lifecycleCount() int {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	return tracer.lifecycleBatches
}

func (tracer *phase61QueryTracer) relationCounts() (int, int) {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	return tracer.dependencyBatches, tracer.childBatches
}

func TestBlueprintAnchorReconciliationStaysQuietIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	parent := phase61Task(workspace, "reconcile-parent-"+suffix, core.TaskQueued, "")
	child := phase61Task(workspace, "reconcile-child-"+suffix, core.TaskQueued, parent.ID)
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	finishDispatchJob(t, st, workspace, parent.ID)
	for tick := 1; tick <= 2; tick++ {
		repaired, err := st.ReconcileQueuedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if repaired != 0 {
			t.Fatalf("tick %d repaired=%d, want 0 for blueprint anchor", tick, repaired)
		}
	}
	var parentEvents int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='dispatch.reconciled'`, parent.ID).Scan(&parentEvents); err != nil {
		t.Fatal(err)
	}
	if parentJob := loadDispatchJob(t, st, workspace, parent.ID); parentJob.Active() || parentEvents != 0 {
		t.Fatalf("blueprint parent job=%+v reconciled events=%d, want inactive/0", parentJob, parentEvents)
	}

	ordinary := phase61Task(workspace, "ordinary-"+suffix, core.TaskQueued, "")
	if err := st.CreateTask(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	finishDispatchJob(t, st, workspace, ordinary.ID)
	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 1 {
		t.Fatalf("ordinary queued reconcile repaired=%d err=%v, want 1", repaired, err)
	}
}

func TestReconcileRescuerDiscardParksRunningTaskWithFailureEvidenceIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	task := phase61Task(workspace, "rescuer-discard-"+core.NewTaskID(), core.TaskQueued, "")
	task.NextStage = core.StageTriage
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	discardDispatchJob(t, st, workspace, task.ID)

	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 1 {
		t.Fatalf("reconciled=%d err=%v, want 1", repaired, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskParked || current.RecoveryStage != core.StageTriage {
		t.Fatalf("task=%+v err=%v, want recoverable parked triage task", current, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundFailure := false
	for _, event := range events {
		if event.Kind == "dispatch.failed" && strings.Contains(string(event.Payload), "discarded the dispatch job") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatal("rescuer discard did not retain dispatch.failed evidence")
	}
	if repaired, err = st.ReconcileQueuedTasks(ctx); err != nil || repaired != 0 {
		t.Fatalf("second reconcile=%d err=%v, want idempotent 0", repaired, err)
	}
}

func TestBlueprintCloseAfterRecoveryAndPeriodicReconcileIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	parent := phase61Task(workspace, "recover-parent-"+suffix, core.TaskQueued, "")
	child := phase61Task(workspace, "recover-child-"+suffix, core.TaskQueued, parent.ID)
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, parent.ID, taskops.Command{
		Kind: core.TaskDispatchFailFinal, RecoveryStage: core.StageImplement, ProjectStages: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}
	parked, err := st.GetTask(ctx, parent.ID)
	if err != nil || parked.State != core.TaskParked {
		t.Fatalf("parent before recovery=%+v err=%v", parked, err)
	}
	recovered, err := taskops.New(st).Perform(ctx, parent.ID, taskops.Command{
		Kind: core.TaskRecover, NextStage: core.StageImplement, ProjectStages: true,
	})
	if err != nil || recovered.Task.State != core.TaskClosed {
		t.Fatalf("recovered parent=%+v err=%v", recovered.Task, err)
	}
	assertBlueprintCloseEvents(t, ctx, st, parent.ID, 1)

	driftParent := phase61Task(workspace, "periodic-parent-"+suffix, core.TaskQueued, "")
	driftChild := phase61Task(workspace, "periodic-child-"+suffix, core.TaskClosed, driftParent.ID)
	if err = st.CreateTask(ctx, driftParent); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(ctx, driftChild); err != nil {
		t.Fatal(err)
	}
	closed, err := st.ReconcileBlueprintClosures(ctx)
	if err != nil || closed != 1 {
		t.Fatalf("periodic close count=%d err=%v, want 1", closed, err)
	}
	closed, err = st.ReconcileBlueprintClosures(ctx)
	if err != nil || closed != 0 {
		t.Fatalf("second periodic close count=%d err=%v, want 0", closed, err)
	}
	assertBlueprintCloseEvents(t, ctx, st, driftParent.ID, 1)
}

func TestBlueprintCloseSerializesWithParentCancelIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	parent := phase61Task(workspace, "cancel-parent-"+suffix, core.TaskQueued, "")
	child := phase61Task(workspace, "cancel-child-"+suffix, core.TaskQueued, parent.ID)
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	go func() {
		_, err := taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskCancel})
		results <- err
	}()
	go func() {
		_, err := taskops.New(st).Cancel(ctx, core.Intervention{
			TaskID: parent.ID, Action: core.InterventionCancel, ReasonCode: "race-test",
		})
		results <- err
	}()
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, store.ErrTaskTerminal) {
			t.Fatal(err)
		}
	}
	persisted, err := st.GetTask(ctx, parent.ID)
	if err != nil || persisted.State != core.TaskClosed {
		t.Fatalf("parent after race=%+v err=%v", persisted, err)
	}
	var terminalTransitions, blueprintEvents int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='task.state_changed'
		  AND payload_json->>'from'='queued' AND payload_json->>'to'='closed'`,
		parent.ID).Scan(&terminalTransitions); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='blueprint.closed'`, parent.ID).Scan(&blueprintEvents); err != nil {
		t.Fatal(err)
	}
	if terminalTransitions != 1 || blueprintEvents > 1 {
		t.Fatalf("terminal transitions=%d blueprint.closed=%d, want 1 and at most 1", terminalTransitions, blueprintEvents)
	}
}

func TestBlueprintParentForeignKeyIsWorkspaceScopedIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	parent := phase61Task(workspace, "fk-parent-"+suffix, core.TaskClosed, "")
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	sameWorkspaceChild := phase61Task(workspace, "fk-valid-child-"+suffix, core.TaskClosed, parent.ID)
	if err := st.CreateTask(ctx, sameWorkspaceChild); err != nil {
		t.Fatalf("same-workspace parent reference: %v", err)
	}

	otherWorkspace := "phase61-other-" + suffix
	otherCtx := store.WithWorkspace(context.Background(), otherWorkspace)
	if _, err := st.BootstrapWorkspaceConfig(otherCtx, &config.Config{
		Workspace: otherWorkspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	crossWorkspaceChild := phase61Task(otherWorkspace, "fk-invalid-child-"+suffix, core.TaskClosed, parent.ID)
	err := st.CreateTask(otherCtx, crossWorkspaceChild)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("cross-workspace parent reference error=%v, want foreign-key violation", err)
	}
}

func TestPhase61BlueprintAndTransactionalDependencyGateIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase61-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	parent := core.Task{
		ID: core.NewTaskID(), Workspace: workspace, Title: "Blueprint", Repo: "conveyor",
		BaseBranch: "main", Branch: "conveyor/task-" + core.NewTaskID(),
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Content: "blueprint", Acceptance: core.JSONPayload([]any{}),
		Decomposition: core.JSONPayload([]core.BlueprintDecompositionItem{
			{ID: "SUB-1", Repo: "conveyor", Summary: "persistence", DependsOn: []string{}},
			{ID: "SUB-2", Repo: "conveyor", Summary: "runtime", DependsOn: []string{"SUB-1"}},
			{ID: "SUB-3", Repo: "conveyor", Summary: "ui", DependsOn: []string{"SUB-2"}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	children, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version)
	if err != nil || len(children) != 3 {
		t.Fatalf("children=%+v err=%v", children, err)
	}
	bySub := map[string]core.Task{}
	for _, child := range children {
		bySub[child.OriginSubID] = child
	}
	if _, err = st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	var childCount, linkCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE workspace_id=$1 AND parent_task_id=$2`, workspace, parent.ID).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE workspace_id=$1 AND src_type='blueprint_version'`, workspace).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 3 || linkCount != 3 {
		t.Fatalf("children=%d blueprint links=%d", childCount, linkCount)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO task_dependencies (workspace_id,task_id,depends_on_task_id) VALUES ($1,$2,$3)`,
		workspace, bySub["SUB-1"].ID, bySub["SUB-2"].ID); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("cycle trigger error=%v", err)
	}
	job := core.Job{ID: "phase61-order", TaskID: bySub["SUB-2"].ID, Stage: core.StageImplement, State: core.JobPending}
	queueEnteredAt := time.Now().UTC().Truncate(time.Microsecond)
	order := core.WorkOrder{
		ID: "phase61-order", TaskID: job.TaskID, JobID: job.ID, Stage: job.Stage,
		State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: queueEnteredAt,
		QueueDeadline: queueEnteredAt.Add(time.Hour),
	}
	if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "blocked", ClientToken: "secret"}); err == nil || !strings.Contains(err.Error(), bySub["SUB-1"].ID) {
		t.Fatalf("transactional blocked claim error=%v", err)
	}
	if blocked, getErr := st.GetWorkOrder(ctx, order.ID); getErr != nil || blocked.QueueBlockedAt.IsZero() ||
		!blocked.QueueEnteredAt.Equal(queueEnteredAt) || blocked.Claimable {
		t.Fatalf("blocked queue clock=%+v err=%v", blocked, getErr)
	}
	transitionPhase61TaskToMerged(t, ctx, st, bySub["SUB-1"].ID)
	if resumed, getErr := st.GetWorkOrder(ctx, order.ID); getErr != nil || !resumed.QueueBlockedAt.IsZero() ||
		!resumed.QueueEnteredAt.Equal(queueEnteredAt) || !resumed.Claimable {
		t.Fatalf("resumed queue clock=%+v err=%v", resumed, getErr)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "ready", ClientToken: "secret"}); err != nil {
		t.Fatalf("claim after dependency merge: %v", err)
	}
	cancelErrs := make(chan error, 2)
	for _, child := range []core.Task{bySub["SUB-2"], bySub["SUB-3"]} {
		go func(id string) {
			_, cancelErr := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskCancel})
			cancelErrs <- cancelErr
		}(child.ID)
	}
	for range 2 {
		if cancelErr := <-cancelErrs; cancelErr != nil {
			t.Fatal(cancelErr)
		}
	}
	if count, countErr := st.CountEvents(ctx, bySub["SUB-3"].ID, "task.dependency_unsatisfiable"); countErr != nil || count != 1 {
		t.Fatalf("unsatisfiable event count=%d err=%v", count, countErr)
	}
	removal := store.DependencyRemovalRequest{
		TaskID: bySub["SUB-3"].ID, DependsOnTaskID: bySub["SUB-2"].ID,
		Reason: "operator resolved terminal dependency", RequestID: "phase61-unlink",
	}
	if result, removeErr := st.RemoveTaskDependency(ctx, removal); removeErr != nil || !result.Removed {
		t.Fatalf("dependency removal=%+v err=%v", result, removeErr)
	}
	if result, removeErr := st.RemoveTaskDependency(ctx, removal); removeErr != nil || result.Removed {
		t.Fatalf("dependency removal retry=%+v err=%v", result, removeErr)
	}
	closed, err := st.GetTask(ctx, parent.ID)
	if err != nil || closed.State != core.TaskClosed {
		t.Fatalf("parent=%+v err=%v", closed, err)
	}
	var closeEventCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id=$1 AND kind='blueprint.closed'`,
		parent.ID).Scan(&closeEventCount); err != nil {
		t.Fatal(err)
	}
	if closeEventCount != 1 {
		t.Fatalf("blueprint.closed events=%d, want 1", closeEventCount)
	}
}

func TestBlueprintApprovalWithoutDecompositionWritesNoMaterializationRowsIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "blueprint-empty-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	parentID := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{
		ID: parentID, Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-" + parentID,
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parentID, Content: "no decomposition", Acceptance: core.JSONPayload([]any{}),
		Decomposition: core.JSONPayload([]any{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, parentID, spec.Version); err != nil {
		t.Fatal(err)
	}
	var children, edges, materializedLinks, materializedEvents int
	for query, destination := range map[string]*int{
		`SELECT count(*) FROM tasks WHERE workspace_id=$1 AND parent_task_id=$2`:                                                          &children,
		`SELECT count(*) FROM task_dependencies WHERE workspace_id=$1 AND (task_id=$2 OR depends_on_task_id=$2)`:                          &edges,
		`SELECT count(*) FROM links WHERE workspace_id=$1 AND kind='materializes' AND (src_id LIKE $2 || ':%' OR src_id=$2 OR dst_id=$2)`: &materializedLinks,
		`SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='blueprint.materialized'`:                              &materializedEvents,
	} {
		if err = st.pool.QueryRow(ctx, query, workspace, parentID).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if children != 0 || edges != 0 || materializedLinks != 0 || materializedEvents != 0 {
		t.Fatalf("children=%d edges=%d materialized_links=%d materialized_events=%d, want all zero",
			children, edges, materializedLinks, materializedEvents)
	}
}

func TestListTasksBatchesRelationHydrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &phase61QueryTracer{}
	poolConfig.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "relation-batch-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	dependency := phase61Task(workspace, "dependency-"+core.NewTaskID(), core.TaskRunning, "")
	parent := phase61Task(workspace, "parent-"+core.NewTaskID(), core.TaskRunning, "")
	child := phase61Task(workspace, "child-"+core.NewTaskID(), core.TaskRunning, parent.ID)
	for _, task := range []core.Task{dependency, parent} {
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO github_lifecycles
		(workspace_id,task_id,repository,spec_version,state) VALUES ($1,$2,$3,1,'queued')`,
		workspace, dependency.ID, "acme/conveyor"); err != nil {
		t.Fatal(err)
	}

	tracer.reset()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dependencyQueries, childQueries := tracer.relationCounts()
	if dependencyQueries != 1 || childQueries != 1 {
		t.Fatalf("relation hydration queries dependency=%d child=%d, want 1/1", dependencyQueries, childQueries)
	}
	if got := tracer.lifecycleCount(); got != 1 {
		t.Fatalf("lifecycle hydration queries=%d want 1", got)
	}
	byID := make(map[string]core.Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if !reflect.DeepEqual(byID[child.ID].Dependencies, []core.TaskRelation{{
		ID: dependency.ID, Title: dependency.Title, State: dependency.State,
	}}) || !reflect.DeepEqual(byID[child.ID].BlockingTaskIDs, []string{dependency.ID}) {
		t.Fatalf("child relations=%+v blockers=%v", byID[child.ID].Dependencies, byID[child.ID].BlockingTaskIDs)
	}
	if len(byID[parent.ID].Children) != 1 || byID[parent.ID].Children[0].ID != child.ID {
		t.Fatalf("parent children=%+v", byID[parent.ID].Children)
	}
	if byID[dependency.ID].GitHub == nil || byID[dependency.ID].GitHub.Repository != "acme/conveyor" || byID[parent.ID].GitHub != nil {
		t.Fatalf("lifecycle hydration dependency=%+v parent=%+v", byID[dependency.ID].GitHub, byID[parent.ID].GitHub)
	}

	emptyWorkspace := "relation-empty-" + core.NewTaskID()
	emptyCtx := store.WithWorkspace(t.Context(), emptyWorkspace)
	if _, err = st.BootstrapWorkspaceConfig(emptyCtx, &config.Config{
		Workspace: emptyWorkspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		task := phase61Task(emptyWorkspace, core.NewTaskID(), core.TaskRunning, "")
		if err = st.CreateTask(emptyCtx, task); err != nil {
			t.Fatal(err)
		}
	}
	tracer.reset()
	emptyTasks, err := st.ListTasks(emptyCtx)
	if err != nil {
		t.Fatal(err)
	}
	dependencyQueries, childQueries = tracer.relationCounts()
	if dependencyQueries != 0 || childQueries != 0 {
		t.Fatalf("empty relation hydration queries dependency=%d child=%d, want 0/0", dependencyQueries, childQueries)
	}
	if got := tracer.lifecycleCount(); got != 1 {
		t.Fatalf("empty-relation lifecycle hydration queries=%d want 1", got)
	}
	for _, task := range emptyTasks {
		if task.Dependencies != nil || task.BlockingTaskIDs != nil || task.Children != nil {
			t.Fatalf("empty relation representation changed for task %s: dependencies=%v blockers=%v children=%v",
				task.ID, task.Dependencies, task.BlockingTaskIDs, task.Children)
		}
	}
}

func TestNewLinksReferenceCommittedEventIdentitiesIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	dependency := phase61Task(workspace, "direct-dependency-"+core.NewTaskID(), core.TaskRunning, "")
	dependent := phase61Task(workspace, "direct-dependent-"+core.NewTaskID(), core.TaskRunning, "")
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTaskWithDependencies(ctx, dependent, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	var directEventID int64
	var directEventTaskID, directEventKind string
	if err := st.pool.QueryRow(ctx, `SELECT link.created_by_event_id,event.task_id,event.kind
		FROM links link
		JOIN events event
		  ON event.workspace_id=link.workspace_id AND event.id=link.created_by_event_id
		WHERE link.workspace_id=$1 AND link.src_type='task' AND link.src_id=$2
		  AND link.dst_type='task' AND link.dst_id=$3 AND link.kind='depends_on'
		  AND link.legacy_created_by_event IS NULL`,
		workspace, dependent.ID, dependency.ID).
		Scan(&directEventID, &directEventTaskID, &directEventKind); err != nil {
		t.Fatal(err)
	}
	if directEventID == 0 || directEventTaskID != dependent.ID || directEventKind != "task.dependency_added" {
		t.Fatalf("direct provenance event=%d task=%s kind=%s", directEventID, directEventTaskID, directEventKind)
	}

	parent := phase61Task(workspace, "provenance-parent-"+core.NewTaskID(), core.TaskRunning, "")
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Acceptance: core.JSONPayload([]any{}),
		Decomposition: core.JSONPayload([]core.BlueprintDecompositionItem{
			{ID: "SUB-1", Repo: "conveyor", Summary: "one"},
			{ID: "SUB-2", Repo: "conveyor", Summary: "two", DependsOn: []string{"SUB-1"}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	children, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version)
	if err != nil || len(children) != 2 {
		t.Fatalf("materialize children=%d err=%v", len(children), err)
	}
	var linkCount, resolvedCount, provenanceEvents int
	if err = st.pool.QueryRow(ctx, `SELECT
			count(*),
			count(*) FILTER (WHERE event.id IS NOT NULL AND link.legacy_created_by_event IS NULL),
			count(DISTINCT event.id)
		FROM links link
		LEFT JOIN events event
		  ON event.workspace_id=link.workspace_id AND event.id=link.created_by_event_id
		WHERE link.workspace_id=$1
		  AND (
		    (link.src_type='blueprint_version' AND link.src_id=$2)
		    OR
		    (link.src_type='task' AND link.src_id IN (
		        SELECT id FROM tasks WHERE workspace_id=$1 AND parent_task_id=$3
		    ))
		  )`,
		workspace, parent.ID+":v1", parent.ID).
		Scan(&linkCount, &resolvedCount, &provenanceEvents); err != nil {
		t.Fatal(err)
	}
	if linkCount != 3 || resolvedCount != 3 || provenanceEvents != 3 {
		t.Fatalf("materialization links=%d resolved=%d distinct_events=%d, want 3/3/3",
			linkCount, resolvedCount, provenanceEvents)
	}
}

func TestLinkProvenanceMigrationPreservesAmbiguousLegacyRowsIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "link_provenance_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateControlPlaneToVersion(t.Context(), pool, 44); err != nil {
		t.Fatalf("migrate isolated schema to version 44: %v", err)
	}
	workspace := "link-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	st := newStore(pool)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	resolvedSource := phase61Task(workspace, "resolved-source-"+core.NewTaskID(), core.TaskRunning, "")
	ambiguousSource := phase61Task(workspace, "ambiguous-source-"+core.NewTaskID(), core.TaskRunning, "")
	destination := phase61Task(workspace, "destination-"+core.NewTaskID(), core.TaskRunning, "")
	for _, task := range []core.Task{resolvedSource, ambiguousSource, destination} {
		if _, err = st.queries.InsertTask(ctx, taskInsertParams(task)); err != nil {
			t.Fatal(err)
		}
	}
	var resolvedEventID int64
	if err = pool.QueryRow(ctx, `INSERT INTO events
		(workspace_id,task_id,kind,actor_id,actor_role,payload_json)
		VALUES ($1,$2,'task.created','migration-test','system','{}')
		RETURNING id`, workspace, resolvedSource.ID).Scan(&resolvedEventID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = pool.Exec(ctx, `INSERT INTO events
			(workspace_id,task_id,kind,actor_id,actor_role,payload_json)
			VALUES ($1,$2,'task.created','migration-test','system','{}')`,
			workspace, ambiguousSource.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, sourceID := range []string{resolvedSource.ID, ambiguousSource.ID} {
		if _, err = pool.Exec(ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event)
			VALUES ($1,'task',$2,'task',$3,'depends_on','task.created')`,
			workspace, sourceID, destination.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = migrateControlPlaneToVersion(t.Context(), pool, 45); err != nil {
		t.Fatalf("migrate isolated schema to version 45: %v", err)
	}
	var migratedEventID *int64
	var legacyKind *string
	if err = pool.QueryRow(ctx, `SELECT created_by_event_id,legacy_created_by_event
		FROM links WHERE workspace_id=$1 AND src_id=$2`, workspace, resolvedSource.ID).
		Scan(&migratedEventID, &legacyKind); err != nil {
		t.Fatal(err)
	}
	if migratedEventID == nil || *migratedEventID != resolvedEventID || legacyKind != nil {
		t.Fatalf("resolved migration event=%v legacy=%v, want %d/nil", migratedEventID, legacyKind, resolvedEventID)
	}
	if err = pool.QueryRow(ctx, `SELECT created_by_event_id,legacy_created_by_event
		FROM links WHERE workspace_id=$1 AND src_id=$2`, workspace, ambiguousSource.ID).
		Scan(&migratedEventID, &legacyKind); err != nil {
		t.Fatal(err)
	}
	if migratedEventID != nil || legacyKind == nil || *legacyKind != "task.created" {
		t.Fatalf("ambiguous migration event=%v legacy=%v, want nil/task.created", migratedEventID, legacyKind)
	}
}

func TestConcurrentReciprocalDependencyEdgesCannotBothCommitIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	first := phase61Task(workspace, "cycle-first-"+core.NewTaskID(), core.TaskRunning, "")
	second := phase61Task(workspace, "cycle-second-"+core.NewTaskID(), core.TaskRunning, "")
	for _, task := range []core.Task{first, second} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	firstTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer firstTx.Rollback(ctx) //nolint:errcheck -- commit owns the successful path
	if _, err = firstTx.Exec(ctx, `INSERT INTO task_dependencies
		(workspace_id,task_id,depends_on_task_id) VALUES ($1,$2,$3)`,
		workspace, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	secondTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer secondTx.Rollback(ctx) //nolint:errcheck -- expected rejection owns this path
	secondResult := make(chan error, 1)
	go func() {
		_, insertErr := secondTx.Exec(ctx, `INSERT INTO task_dependencies
			(workspace_id,task_id,depends_on_task_id) VALUES ($1,$2,$3)`,
			workspace, second.ID, first.ID)
		secondResult <- insertErr
	}()
	if err = firstTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-secondResult; err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("reciprocal insert error=%v, want dependency cycle", err)
	}
	var edgeCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM task_dependencies
		WHERE workspace_id=$1 AND (
		  (task_id=$2 AND depends_on_task_id=$3)
		  OR (task_id=$3 AND depends_on_task_id=$2)
		)`, workspace, first.ID, second.ID).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if edgeCount != 1 {
		t.Fatalf("reciprocal committed edges=%d, want exactly one", edgeCount)
	}
}

func TestPlanningBundleApprovalTransactionIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-bundle-" + core.NewTaskID(), Title: "Bundle"}, core.RequirementVersion{Content: "Bundle", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Create bundled tasks."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	pending, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "Bundle v2", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Create dependency-ordered bundled tasks."}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: pending.Version}}, Tasks: []core.PlanningBundleTask{
		{MemberID: "one", Title: "One", Body: "One", Repo: "conveyor", Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
		{MemberID: "two", Title: "Two", Body: "Two", Repo: "conveyor", DependsOn: []string{"one"}, Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
		{MemberID: "three", Title: "Three", Body: "Three", Repo: "conveyor", DependsOn: []string{"two"}, Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := st.ApprovePlanningBundle(ctx, bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApprovePlanningBundle(ctx, bundle.ID); err != nil {
		t.Fatal(err)
	}
	var tasks, edges, contexts int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE workspace_id=$1 AND id=ANY($2)`, workspace, []string{approved.Tasks[0].CreatedTaskID, approved.Tasks[1].CreatedTaskID, approved.Tasks[2].CreatedTaskID}).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM task_dependencies WHERE workspace_id=$1 AND task_id=ANY($2)`, workspace, []string{approved.Tasks[0].CreatedTaskID, approved.Tasks[1].CreatedTaskID, approved.Tasks[2].CreatedTaskID}).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=ANY($2) AND kind=$3`, workspace, []string{approved.Tasks[0].CreatedTaskID, approved.Tasks[1].CreatedTaskID, approved.Tasks[2].CreatedTaskID}, store.TaskContextRequirementAdded).Scan(&contexts); err != nil {
		t.Fatal(err)
	}
	if tasks != 3 || edges != 2 || contexts != 3 {
		t.Fatalf("tasks=%d edges=%d contexts=%d", tasks, edges, contexts)
	}
	version, err := st.GetRequirementVersion(ctx, requirement.ID, pending.Version)
	if err != nil || version.Confirmed {
		t.Fatalf("pending=%+v err=%v", version, err)
	}
	if _, err = st.RebuildLineage(ctx, core.LineageRebuildRequest{RequestID: "bundle-rebuild-" + core.NewTaskID(), Reason: "bundle integration proof"}); err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposedLinks, taskLinks int
	for _, link := range links {
		if link.SrcType == core.LineagePlanningBundle && link.SrcID == bundle.ID {
			if link.Kind == "proposes" {
				proposedLinks++
			}
			if link.Kind == "creates" {
				taskLinks++
			}
		}
	}
	if proposedLinks != 1 || taskLinks != 3 {
		t.Fatalf("rebuilt bundle lineage proposed=%d tasks=%d", proposedLinks, taskLinks)
	}

	existing := phase61Task(workspace, "collision-"+core.NewTaskID(), core.TaskQueued, "")
	if err = st.CreateTask(ctx, existing); err != nil {
		t.Fatal(err)
	}
	failureSession, _ := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-failure-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
	failing, err := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "bundle-failure-" + core.NewTaskID(), SessionID: failureSession.ID, Title: "Failure", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: pending.Version}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", CreatedTaskID: existing.ID, Title: "Collision", Body: "Collision", Repo: "conveyor"}, {MemberID: "two", Title: "Must roll back", Body: "Must roll back", Repo: "conveyor", DependsOn: []string{"one"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApprovePlanningBundle(ctx, failing.ID); err == nil {
		t.Fatal("colliding approval unexpectedly succeeded")
	}
	stored, err := st.GetPlanningBundle(ctx, failing.ID)
	if err != nil || stored.Status != core.PlanningBundlePending {
		t.Fatalf("failed bundle=%+v err=%v", stored, err)
	}
	if _, err = st.GetTask(ctx, failing.Tasks[1].CreatedTaskID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("partial task persisted: %v", err)
	}
}

func newPhase61IntegrationStore(t *testing.T) (*Store, context.Context, string) {
	t.Helper()
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	workspace := "phase61-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, ctx, workspace
}

func phase61Task(workspace, id string, state core.TaskState, parentID string) core.Task {
	task := core.Task{
		ID: id, Workspace: workspace, Title: id, Repo: "conveyor",
		BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: state, NextStage: core.StageImplement, ParentTaskID: parentID,
		CreatedAt: time.Now().UTC(),
	}
	if parentID != "" {
		task.OriginSpecVersion = 1
		task.OriginSubID = "SUB-1"
	}
	return task
}

func assertBlueprintCloseEvents(t *testing.T, ctx context.Context, st *Store, taskID string, want int) {
	t.Helper()
	var transitions, closedEvents int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='task.state_changed'
		  AND payload_json->>'command'='blueprint.close'`, taskID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='blueprint.closed'`, taskID).Scan(&closedEvents); err != nil {
		t.Fatal(err)
	}
	if transitions != want || closedEvents != want {
		t.Fatalf("task %s blueprint transitions=%d events=%d, want %d/%d", taskID, transitions, closedEvents, want, want)
	}
}

func transitionPhase61TaskToMerged(t *testing.T, ctx context.Context, st store.Store, id string) {
	t.Helper()
	for _, command := range []taskops.Command{
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true},
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskGateMerge},
		{Kind: core.TaskInterventionApproveReview},
		{Kind: core.TaskMergeConfirm},
	} {
		if _, err := taskops.New(st).Perform(ctx, id, command); err != nil {
			t.Fatalf("%s: %v", command.Kind, err)
		}
	}
}

// finishDispatchJob completes a task's dispatch job the way a worker would,
// leaving the stream inactive.
func finishDispatchJob(t *testing.T, st *Store, workspace, taskID string) {
	t.Helper()
	job := loadDispatchJob(t, st, workspace, taskID)
	if !job.Active() {
		return
	}
	head := job.Head
	if job.State != logqueue.StateRunning {
		appendQueueEvent(t, st, workspace, job.Stream, head, logqueue.KindClaimed, map[string]any{"attempt": job.Attempt + 1, "worker": "test", "claimed_at": time.Now().UTC()})
		head++
	}
	appendQueueEvent(t, st, workspace, job.Stream, head, logqueue.KindCompleted, map[string]any{"attempt": job.Attempt + 1})
}

// discardDispatchJob exhausts a task's dispatch job: every attempt fails
// and the last is discarded.
func discardDispatchJob(t *testing.T, st *Store, workspace, taskID string) {
	t.Helper()
	job := loadDispatchJob(t, st, workspace, taskID)
	head := job.Head
	attempt := job.Attempt
	if job.State != logqueue.StateRunning {
		attempt++
		appendQueueEvent(t, st, workspace, job.Stream, head, logqueue.KindClaimed, map[string]any{"attempt": attempt, "worker": "test", "claimed_at": time.Now().UTC()})
		head++
	}
	for attempt < job.MaxAttempts {
		appendQueueEvent(t, st, workspace, job.Stream, head, logqueue.KindFailed, map[string]any{"attempt": attempt, "error": "boom", "next_at": time.Now().UTC()})
		attempt++
		appendQueueEvent(t, st, workspace, job.Stream, head+1, logqueue.KindClaimed, map[string]any{"attempt": attempt, "worker": "test", "claimed_at": time.Now().UTC()})
		head += 2
	}
	appendQueueEvent(t, st, workspace, job.Stream, head, logqueue.KindDiscarded, map[string]any{"attempt": attempt, "error": "boom"})
}

func loadDispatchJob(t *testing.T, st *Store, workspace, taskID string) logqueue.Job {
	t.Helper()
	job, err := logqueue.Load(t.Context(), st.Log(), workspace, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), taskID))
	if err != nil {
		t.Fatal(err)
	}
	return job
}
