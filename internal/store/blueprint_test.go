package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestBlueprintMaterializationDependencyClaimsAndParentClose(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemoryWithConfig(&config.Config{
		Workspace: "demo",
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	parent := core.Task{
		ID: "blueprint", Workspace: "demo", Title: "Blueprint", Repo: "conveyor",
		BaseBranch: "main", Branch: "conveyor/task-blueprint", State: core.TaskQueued,
		NextStage: core.StageImplement, SpecApproval: true, MergeApproval: true,
		PolicyVersion: 1, SetupName: "default", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Content: "approved blueprint", Acceptance: core.JSONPayload([]any{}),
		Decomposition: core.JSONPayload([]blueprintDecompositionItem{
			{ID: "SUB-1", Repo: "conveyor", Summary: "Persistence", DependsOn: []string{}},
			{ID: "SUB-2", Repo: "conveyor", Summary: "Materialization", DependsOn: []string{"SUB-1"}},
			{ID: "SUB-3", Repo: "conveyor", Summary: "UI", DependsOn: []string{"SUB-2"}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	children, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 {
		t.Fatalf("children=%d want=3", len(children))
	}
	bySub := map[string]core.Task{}
	for _, child := range children {
		bySub[child.OriginSubID] = child
		if child.ParentTaskID != parent.ID || child.OriginSpecVersion != spec.Version ||
			child.NextStage != core.StageImplement || child.State != core.TaskQueued {
			t.Fatalf("invalid child: %+v", child)
		}
	}
	repeated, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version)
	if err != nil || len(repeated) != 3 {
		t.Fatalf("repeat children=%d err=%v", len(repeated), err)
	}
	all, _ := st.ListTasks(ctx)
	if len(all) != 4 {
		t.Fatalf("tasks=%d want parent plus three idempotent children", len(all))
	}
	blocking, err := st.ListBlockingTaskIDs(ctx, bySub["SUB-2"].ID)
	if err != nil || len(blocking) != 1 || blocking[0] != bySub["SUB-1"].ID {
		t.Fatalf("blocking=%v err=%v", blocking, err)
	}
	order := core.WorkOrder{
		ID: "sub-2-order", TaskID: bySub["SUB-2"].ID, JobID: "sub-2-job",
		Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true,
		QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().Add(time.Hour),
	}
	if err = st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: order.TaskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, order)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).ClaimWorkOrder(ctx, order.TaskID, order.ID, core.WorkOrderClaim{SessionID: "blocked", ClientToken: "secret"}); err == nil || !strings.Contains(err.Error(), bySub["SUB-1"].ID) {
		t.Fatalf("blocked claim error=%v", err)
	}
	transitionTaskToMerged(t, ctx, st, bySub["SUB-1"].ID)
	if blocking, err = st.ListBlockingTaskIDs(ctx, bySub["SUB-2"].ID); err != nil || len(blocking) != 0 {
		t.Fatalf("blocking after merge=%v err=%v", blocking, err)
	}
	if _, err = taskops.New(st).ClaimWorkOrder(ctx, order.TaskID, order.ID, core.WorkOrderClaim{SessionID: "unblocked", ClientToken: "secret"}); err != nil {
		t.Fatalf("claim after merge: %v", err)
	}
	for _, child := range []core.Task{bySub["SUB-2"], bySub["SUB-3"]} {
		if _, err = taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := st.GetTask(ctx, parent.ID)
	if err != nil || closed.State != core.TaskClosed {
		t.Fatalf("parent=%+v err=%v", closed, err)
	}
}

func transitionTaskToMerged(t *testing.T, ctx context.Context, st Store, id string) {
	t.Helper()
	commands := []taskops.Command{
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true},
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskGateMerge},
		{Kind: core.TaskInterventionApproveReview},
		{Kind: core.TaskMergeConfirm},
	}
	for _, command := range commands {
		if _, err := taskops.New(st).Perform(ctx, id, command); err != nil {
			t.Fatalf("%s on %s: %v", command.Kind, id, err)
		}
	}
}

func TestBlueprintCycleFailsApprovalWithoutPartialState(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemoryWithConfig(&config.Config{
		Workspace: "demo",
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	parent := core.Task{ID: "cycle-parent", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-cycle-parent", State: core.TaskQueued}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: parent.ID, Decomposition: core.JSONPayload([]blueprintDecompositionItem{
		{ID: "SUB-1", Repo: "conveyor", Summary: "one", DependsOn: []string{"SUB-2"}},
		{ID: "SUB-2", Repo: "conveyor", Summary: "two", DependsOn: []string{"SUB-1"}},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error=%v", err)
	}
	latest, ok, err := st.GetLatestSpecVersion(ctx, parent.ID)
	if err != nil || !ok || latest.Approved {
		t.Fatalf("latest=%+v ok=%v err=%v", latest, ok, err)
	}
	tasks, _ := st.ListTasks(ctx)
	if len(tasks) != 1 {
		t.Fatalf("partial tasks=%d", len(tasks))
	}
}
