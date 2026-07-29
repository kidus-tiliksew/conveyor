package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

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
