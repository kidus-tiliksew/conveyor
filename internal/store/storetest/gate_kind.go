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
// Dispatch, plan revision, review aggregation, request changes, cancel, start
// over, and conflict fix each have their own assignment. ListTasks is the
// readback, and AuditTaskGateKinds is the report.
func RunGateKindConformance(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	plane := taskops.New(st)
	cases := []struct {
		command core.TaskCommand
		want    core.GateKind
	}{
		{core.TaskGateSpec, core.GateKindPlanApproval},
		{core.TaskJobFail, core.GateKindJobFailure},
	}
	for _, tc := range cases {
		t.Run(string(tc.command), func(t *testing.T) {
			id := createRunningTask(t, st, ctx, workspace)
			out, err := plane.Perform(ctx, id, taskops.Command{Kind: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			assertGate(t, st, ctx, id, out.Task, core.TaskAwaiting, tc.want)
		})
	}
	t.Run("plan revision", func(t *testing.T) {
		fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
		result, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "the API moved")
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, fixture.order.TaskID, result.Task, core.TaskAwaiting, core.GateKindPlanRevision)
	})
	t.Run("review aggregation merge gate", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		acceptReviewRound(t, st, ctx, id, func(order core.WorkOrder, seat int) core.ReviewDecision {
			return core.ReviewDecision{
				TaskID: id, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat,
				Verdict: "approve", ReasonCode: "verified", Summary: "merge gate", ReviewedCommitSHA: "head",
				PolicyVersion: 1, MergeApproval: true, MaxBounces: 3,
			}
		})
		stored, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, stored, core.TaskAwaiting, core.GateKindMergeApproval)
	})
	t.Run("review aggregation bounce limit", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		acceptReviewRound(t, st, ctx, id, func(order core.WorkOrder, seat int) core.ReviewDecision {
			return core.ReviewDecision{
				TaskID: id, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat,
				Verdict: "changes_requested", ReasonCode: "fix", Summary: "bounce", Feedback: "again", MaxBounces: 1,
			}
		})
		stored, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, stored, core.TaskAwaiting, core.GateKindReviewBounceLimit)
	})
	t.Run("request changes bounce limit", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		transitionToMergeGate(t, ctx, st, id)
		user := store.WithActor(ctx, store.Actor{ID: "operator-gate", Role: core.ActorUser})
		out, err := taskops.New(st).RequestChanges(user, taskops.RequestChanges{TaskID: id, Feedback: "limit", MaxBounces: 1})
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, out, core.TaskAwaiting, core.GateKindReviewBounceLimit)
	})
	for _, exit := range []struct {
		command core.TaskCommand
		state   core.TaskState
	}{
		{core.TaskInterventionApproveSpec, core.TaskQueued},
		{core.TaskInterventionApproveReview, core.TaskApproved},
		{core.TaskInterventionRedirect, core.TaskQueued},
		{core.TaskInterventionReject, core.TaskClosed},
	} {
		t.Run(string(exit.command), func(t *testing.T) {
			id := createRunningTask(t, st, ctx, workspace)
			if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
				t.Fatal(err)
			}
			out, err := plane.Perform(ctx, id, taskops.Command{Kind: exit.command})
			if err != nil {
				t.Fatal(err)
			}
			assertGate(t, st, ctx, id, out.Task, exit.state, "")
		})
	}
	t.Run("cancel clears the kind", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		out, err := plane.Cancel(ctx, core.Intervention{TaskID: id, Action: core.InterventionCancel, ReasonCode: "operator_cancel"})
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, out, core.TaskClosed, "")
	})
	t.Run("start over clears the kind", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		result, err := plane.StartOver(ctx, core.TaskStartOverRequest{TaskID: id, RequestID: "restart-" + id, Reason: "fresh"})
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, result.Task, core.TaskClosed, "")
	})
	t.Run("conflict fix writes an empty kind", func(t *testing.T) {
		id := createTask(t, st, ctx, workspace, core.TaskApproved, core.StageReview)
		now := time.Now().UTC()
		jobID := id + "-conflict"
		_, err := taskops.ExecuteWorkOrder(ctx, st, id, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ConflictFixResult, error) {
			return st.CreateConflictFixCommand(ctx, lease, store.ConflictFixRequest{
				TaskID: id,
				Job:    core.Job{ID: jobID, TaskID: id, Stage: core.StageImplement, State: core.JobPending},
				WorkOrder: core.WorkOrder{
					ID: jobID, TaskID: id, JobID: jobID, Stage: core.StageImplement, ReasonCode: "merge-conflict",
					QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
				},
				Intervention: core.Intervention{TaskID: id, Action: core.InterventionRedirect, ReasonCode: "merge-conflict"},
				ApprovedHead: "approved", NewHead: "conflicting",
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, st, ctx, id, stored, core.TaskQueued, "")
	})
	t.Run("audit flags a seeded mismatch", func(t *testing.T) {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		if clean := auditTask(t, st, ctx, id); len(clean) != 0 {
			t.Fatalf("clean gate audit=%+v", clean)
		}
		if err := st.AppendEvent(ctx, core.Event{
			TaskID: id, Kind: "task.state_changed", At: time.Now().UTC().Add(time.Minute),
			Payload: core.JSONPayload(map[string]any{"command": core.TaskJobFail}),
		}); err != nil {
			t.Fatal(err)
		}
		violations := auditTask(t, st, ctx, id)
		if len(violations) != 1 || violations[0].Reason != "mismatch" || violations[0].Expected != string(core.GateKindJobFailure) {
			t.Fatalf("seeded audit=%+v", violations)
		}
	})
}

func assertGate(t *testing.T, st store.Store, ctx context.Context, id string, returned core.Task, state core.TaskState, kind core.GateKind) {
	t.Helper()
	if returned.State != state || returned.GateKind != kind {
		t.Fatalf("returned state=%s gate_kind=%q, want %s %q", returned.State, returned.GateKind, state, kind)
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != state || stored.GateKind != kind {
		t.Fatalf("stored state=%s gate_kind=%q, want %s %q", stored.State, stored.GateKind, state, kind)
	}
	listed := listedTask(t, st, ctx, id)
	if listed.State != state || listed.GateKind != kind {
		t.Fatalf("listed state=%s gate_kind=%q, want %s %q", listed.State, listed.GateKind, state, kind)
	}
	if violations := auditTask(t, st, ctx, id); kind == listed.GateKind && len(violations) != 0 {
		t.Fatalf("audit=%+v", violations)
	}
}

func listedTask(t *testing.T, st store.Store, ctx context.Context, id string) core.Task {
	t.Helper()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %s missing from ListTasks", id)
	return core.Task{}
}

func auditTask(t *testing.T, st store.Store, ctx context.Context, id string) []store.TaskGateAuditViolation {
	t.Helper()
	task := listedTask(t, st, ctx, id)
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return store.AuditTaskGateKinds([]store.TaskGateObservation{{
		TaskID: task.ID, State: task.State, GateKind: task.GateKind,
	}}, events)
}

func acceptReviewRound(t *testing.T, st store.Store, ctx context.Context, taskID string, decision func(core.WorkOrder, int) core.ReviewDecision) {
	t.Helper()
	jobs, orders := reviewRound(taskID, 1)
	requireOK(t, CreateReviewRound(ctx, st, taskID, jobs, orders))
	for i, order := range orders {
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(order, i+1)))
	}
}

func createRunningTask(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	return createTask(t, st, ctx, workspace, core.TaskRunning, "")
}

func createTask(t *testing.T, st store.Store, ctx context.Context, workspace string, state core.TaskState, next core.Stage) string {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, NextStage: next, CreatedAt: time.Now().UTC()}
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
