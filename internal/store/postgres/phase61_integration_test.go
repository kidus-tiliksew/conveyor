package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

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
	if _, err := st.pool.Exec(ctx, `DELETE FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`,
		workspace, parent.ID); err != nil {
		t.Fatal(err)
	}
	for tick := 1; tick <= 2; tick++ {
		repaired, err := st.ReconcileQueuedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if repaired != 0 {
			t.Fatalf("tick %d repaired=%d, want 0 for blueprint anchor", tick, repaired)
		}
	}
	var parentJobs, parentEvents int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`,
		workspace, parent.ID).Scan(&parentJobs); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE task_id=$1 AND kind='dispatch.reconciled'`, parent.ID).Scan(&parentEvents); err != nil {
		t.Fatal(err)
	}
	if parentJobs != 0 || parentEvents != 0 {
		t.Fatalf("blueprint parent jobs=%d reconciled events=%d, want 0/0", parentJobs, parentEvents)
	}

	ordinary := phase61Task(workspace, "ordinary-"+suffix, core.TaskQueued, "")
	if err := st.CreateTask(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`,
		workspace, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 1 {
		t.Fatalf("ordinary queued reconcile repaired=%d err=%v, want 1", repaired, err)
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
	ctx := store.WithWorkspace(context.Background(), workspace)
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
		Decomposition: core.JSONPayload([]blueprintDecompositionItem{
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
	order := core.WorkOrder{
		ID: "phase61-order", TaskID: job.TaskID, JobID: job.ID, Stage: job.Stage,
		State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: time.Now().UTC(),
		QueueDeadline: time.Now().Add(time.Hour),
	}
	if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "blocked", ClientToken: "secret"}); err == nil || !strings.Contains(err.Error(), bySub["SUB-1"].ID) {
		t.Fatalf("transactional blocked claim error=%v", err)
	}
	transitionPhase61TaskToMerged(t, ctx, st, bySub["SUB-1"].ID)
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
