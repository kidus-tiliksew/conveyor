package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunGateKindConformance proves a state writer records the gate kind of the
// transition that entered awaiting_human, and clears it on every other write.
func RunGateKindConformance(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	plane := taskops.New(st)
	cases := []struct {
		command core.TaskCommand
		want    core.GateKind
	}{
		{core.TaskGateSpec, core.GateKindPlanApproval},
		{core.TaskGatePlanRevision, core.GateKindPlanRevision},
		{core.TaskGateMerge, core.GateKindMergeApproval},
		{core.TaskStageBounceLimit, core.GateKindReviewBounceLimit},
		{core.TaskJobFail, core.GateKindJobFailure},
	}
	for _, tc := range cases {
		t.Run(string(tc.command), func(t *testing.T) {
			id := createRunningTask(t, st, ctx, workspace)
			out, err := plane.Perform(ctx, id, taskops.Command{Kind: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			if out.Task.State != core.TaskAwaiting || out.Task.GateKind != tc.want {
				t.Fatalf("returned state=%s gate_kind=%q, want awaiting_human %q", out.Task.State, out.Task.GateKind, tc.want)
			}
			stored, err := st.GetTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.GateKind != tc.want {
				t.Fatalf("stored gate_kind=%q, want %q", stored.GateKind, tc.want)
			}
		})
	}
	t.Run("cleared on leave", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		out, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskInterventionApproveSpec})
		if err != nil {
			t.Fatal(err)
		}
		if out.Task.State == core.TaskAwaiting || out.Task.GateKind != "" {
			t.Fatalf("left gate state=%s gate_kind=%q", out.Task.State, out.Task.GateKind)
		}
		stored, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.GateKind != "" {
			t.Fatalf("stored gate_kind=%q after leaving the gate", stored.GateKind)
		}
	})
	t.Run("non-awaiting write is empty", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		out, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskStageAdvance, ProjectStages: true, NextStage: core.StageImplement})
		if err != nil {
			t.Fatal(err)
		}
		if out.Task.State == core.TaskAwaiting || out.Task.GateKind != "" {
			t.Fatalf("advance state=%s gate_kind=%q", out.Task.State, out.Task.GateKind)
		}
	})
}

func createRunningTask(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GateKind != "" {
		t.Fatalf("new task gate_kind=%q", stored.GateKind)
	}
	return id
}
