package storetest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunTerminalOrderRetirementConformance proves every edge into merged or
// closed leaves the task with no non-terminal work order on every backend.
// Each retired order is cancelled under order.cancel with exactly one
// work_order.cancelled event, a failed job, and suppressed retry, while a
// completed order is unchanged (component-task-lifecycle "Terminal retirement
// of work orders"; req-task-lifecycle-and-queue AC-8.1).
func RunTerminalOrderRetirementConformance(t *testing.T, x Fixture) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	ops := taskops.New(st)
	perform := func(t *testing.T, taskID string, commands ...core.TaskCommand) {
		t.Helper()
		for _, command := range commands {
			if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: command}); err != nil {
				t.Fatalf("%s on %s: %v", command, taskID, err)
			}
		}
	}

	t.Run("merge.confirm with a queued implement order and an older stale review seat", func(t *testing.T) {
		taskID := newRetirementTask(t, x, core.TaskQueued, core.StageSpec)
		completed := addRetirementOrder(t, x, taskID, taskID+"-spec-1", core.StageSpec, 0, 0)
		completeRetirementOrder(t, x, completed)
		perform(t, taskID, core.TaskGateMerge, core.TaskInterventionApproveReview)
		queued := addRetirementOrder(t, x, taskID, taskID+"-implement-2", core.StageImplement, 0, 0)
		stale := addRetirementOrder(t, x, taskID, taskID+"-review-1-seat-1", core.StageReview, 1, 1)
		moveRetirementOrder(t, x, stale, core.WorkOrderStale, core.WorkOrderCmdMarkStale)
		timedOut := addRetirementOrder(t, x, taskID, taskID+"-review-2-seat-1", core.StageReview, 2, 1)
		moveRetirementOrder(t, x, timedOut, core.WorkOrderTimedOut, core.WorkOrderCmdTimeout)

		perform(t, taskID, core.TaskMergeConfirm)
		requireTerminalRetirement(t, x, taskID, core.TaskMergeConfirm, []string{queued, stale, timedOut}, []string{completed})
	})

	t.Run("intervention.reject revokes a claimed order and cancels a queued one", func(t *testing.T) {
		taskID := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
		claimed := addRetirementOrder(t, x, taskID, taskID+"-implement-1", core.StageImplement, 0, 0)
		session := "terminal-reject-session"
		if _, err := For(st).ClaimWorkOrder(ctx, claimed, core.WorkOrderClaim{SessionID: session, ClientToken: "terminal-reject-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		queued := addRetirementOrder(t, x, taskID, taskID+"-spec-1", core.StageSpec, 0, 0)
		perform(t, taskID, core.TaskGateSpec, core.TaskInterventionReject)

		requireTerminalRetirement(t, x, taskID, core.TaskInterventionReject, []string{claimed, queued}, nil)
		order, err := st.GetWorkOrder(ctx, claimed)
		requireOK(t, err)
		if order.SessionID != "" || order.AttemptID != "" || order.LastAttemptID == "" || order.LastAttemptOutcome != core.WorkOrderOutcomeCancelled || !order.LeaseExpiresAt.IsZero() {
			t.Fatalf("revoked claim=%+v", order)
		}
		if _, err = For(st).RenewWorkerClaim(ctx, claimed, "", session, time.Hour); err == nil {
			t.Fatal("renewal by the revoked session succeeded")
		}
		if order, err = st.GetWorkOrder(ctx, claimed); err != nil || order.State != core.WorkOrderCancelled {
			t.Fatalf("after refused renewal order=%+v err=%v", order, err)
		}
		if _, err = For(st).ClaimWorkOrder(ctx, queued, core.WorkOrderClaim{SessionID: "late-session", ClientToken: "late-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err == nil {
			t.Fatal("a retired order was claimed")
		}
	})

	t.Run("merge.recover cancels a submitted implement order", func(t *testing.T) {
		taskID := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
		submitted := addRetirementOrder(t, x, taskID, taskID+"-implement-1", core.StageImplement, 0, 0)
		order, err := For(st).ClaimWorkOrder(ctx, submitted, core.WorkOrderClaim{SessionID: "terminal-recover-session", ClientToken: "terminal-recover-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		order.State = core.WorkOrderSubmitted
		requireOK(t, UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdSubmitForReview))
		perform(t, taskID, core.TaskGateMerge, core.TaskMergeRecover)
		requireTerminalRetirement(t, x, taskID, core.TaskMergeRecover, []string{submitted}, nil)
	})

	t.Run("intervention.merge cancels a queued review seat", func(t *testing.T) {
		taskID := newRetirementTask(t, x, core.TaskAwaiting, "")
		queued := addRetirementOrder(t, x, taskID, taskID+"-review-1-seat-1", core.StageReview, 1, 1)
		perform(t, taskID, core.TaskInterventionMerge)
		requireTerminalRetirement(t, x, taskID, core.TaskInterventionMerge, []string{queued}, nil)
	})

	t.Run("blueprint.close cancels the anchor's open order", func(t *testing.T) {
		parentID := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
		queued := addRetirementOrder(t, x, parentID, parentID+"-implement-1", core.StageImplement, 0, 0)
		childID := "terminal-child-" + core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{
			ID: childID, Workspace: x.Workspace, Title: childID, Repo: "conveyor", BaseBranch: "main",
			Branch: "conveyor/task-" + childID, State: core.TaskAwaiting, ParentTaskID: parentID, CreatedAt: time.Now().UTC(),
		}))
		perform(t, childID, core.TaskInterventionReject)
		parent, err := st.GetTask(ctx, parentID)
		requireOK(t, err)
		if parent.State != core.TaskClosed {
			t.Fatalf("anchor state=%s, want closed", parent.State)
		}
		requireTerminalRetirement(t, x, parentID, core.TaskBlueprintClose, []string{queued}, nil)
	})

	t.Run("cancellation shares the sweep", func(t *testing.T) {
		taskID := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
		queued := addRetirementOrder(t, x, taskID, taskID+"-implement-1", core.StageImplement, 0, 0)
		stale := addRetirementOrder(t, x, taskID, taskID+"-spec-1", core.StageSpec, 0, 0)
		moveRetirementOrder(t, x, stale, core.WorkOrderStale, core.WorkOrderCmdMarkStale)
		cancelled, err := ops.Cancel(ctx, core.Intervention{TaskID: taskID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
		requireOK(t, err)
		if cancelled.State != core.TaskClosed {
			t.Fatalf("cancelled task state=%s", cancelled.State)
		}
		requireTerminalRetirement(t, x, taskID, core.TaskCancel, []string{queued, stale}, nil)
	})
}

// RunTerminalOrderClockConformance proves the order clock retires orders that
// history left open on a merged or closed task, without a due transition and
// bounded per tick, never marks them stale or timed_out again, and leaves open
// tasks' clocks and other workspaces unchanged (component-work-orders "Three
// clocks").
func RunTerminalOrderClockConformance(t *testing.T, x Fixture) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	ops := taskops.New(st)
	now := time.Now().UTC()

	orphan := newRetirementTask(t, x, core.TaskMerged, "")
	queued := addRetirementOrder(t, x, orphan, orphan+"-implement-1", core.StageImplement, 0, 0)
	stale := addRetirementOrder(t, x, orphan, orphan+"-review-1-seat-1", core.StageReview, 1, 1)
	moveRetirementOrder(t, x, stale, core.WorkOrderStale, core.WorkOrderCmdMarkStale)
	timedOut := addRetirementOrder(t, x, orphan, orphan+"-review-2-seat-1", core.StageReview, 2, 1)
	moveRetirementOrder(t, x, timedOut, core.WorkOrderTimedOut, core.WorkOrderCmdTimeout)
	closedOrphan := newRetirementTask(t, x, core.TaskClosed, "")
	closedQueued := addRetirementOrder(t, x, closedOrphan, closedOrphan+"-implement-1", core.StageImplement, 0, 0)

	open := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
	openOrder := addRetirementOrder(t, x, open, open+"-spec-1", core.StageSpec, 0, 0)
	elapsed, err := st.GetWorkOrder(ctx, openOrder)
	requireOK(t, err)
	elapsed.QueueDeadline = now.Add(-time.Minute)
	requireOK(t, UpdateWorkOrder(ctx, st, elapsed, taskops.WorkOrderMetadataCommand))

	foreign := x.Workspace + "-terminal-" + core.NewTaskID()[:6]
	away := store.WithWorkspace(ctx, foreign)
	_, err = st.BootstrapWorkspaceConfig(away, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	awayFixture := x
	awayFixture.Context, awayFixture.Workspace = away, foreign
	foreignOrphan := newRetirementTask(t, awayFixture, core.TaskMerged, "")
	foreignQueued := addRetirementOrder(t, awayFixture, foreignOrphan, foreignOrphan+"-implement-1", core.StageImplement, 0, 0)

	now = time.Now().UTC()
	candidates, err := st.ListElapsedWorkOrderTaskIDs(ctx, now)
	requireOK(t, err)
	for _, want := range []string{orphan, closedOrphan, open} {
		if !containsString(candidates, want) {
			t.Fatalf("clock candidates=%v, missing %s", candidates, want)
		}
	}
	if containsString(candidates, foreignOrphan) {
		t.Fatalf("clock candidates=%v include another workspace's task %s", candidates, foreignOrphan)
	}

	if _, err = ops.TickOrderClock(ctx, now); err != nil {
		t.Fatal(err)
	}
	requireTerminalRetirement(t, x, orphan, "", []string{queued, stale, timedOut}, nil)
	requireTerminalRetirement(t, x, closedOrphan, "", []string{closedQueued}, nil)
	if order, getErr := st.GetWorkOrder(ctx, openOrder); getErr != nil || order.State != core.WorkOrderStale {
		t.Fatalf("open task order=%+v err=%v, want stale", order, getErr)
	}
	if order, getErr := st.GetWorkOrder(away, foreignQueued); getErr != nil || order.State != core.WorkOrderQueued {
		t.Fatalf("foreign orphan=%+v err=%v, want untouched until its workspace ticks", order, getErr)
	}

	before := countRetirementEvents(t, x, orphan)
	for tick := 0; tick < 2; tick++ {
		if _, err = ops.TickOrderClock(ctx, now.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if after := countRetirementEvents(t, x, orphan); after != before {
		t.Fatalf("repeated ticks appended work_order.cancelled events: before=%d after=%d", before, after)
	}
	requireTerminalRetirement(t, x, orphan, "", []string{queued, stale, timedOut}, nil)
	candidates, err = st.ListElapsedWorkOrderTaskIDs(ctx, now.Add(48*time.Hour))
	requireOK(t, err)
	if containsString(candidates, orphan) || containsString(candidates, closedOrphan) {
		t.Fatalf("retired orphans are still clock candidates: %v", candidates)
	}

	if _, err = taskops.New(st).TickOrderClock(away, now); err != nil {
		t.Fatal(err)
	}
	requireTerminalRetirement(t, awayFixture, foreignOrphan, "", []string{foreignQueued}, nil)

	t.Run("bounded batch", func(t *testing.T) {
		total := store.TerminalOrphanTaskBatch + 1
		orders, tasks := make([]string, 0, total), make([]string, 0, total)
		for i := 0; i < total; i++ {
			id := newRetirementTask(t, x, core.TaskClosed, "")
			tasks = append(tasks, id)
			orders = append(orders, addRetirementOrder(t, x, id, id+"-implement-1", core.StageImplement, 0, 0))
		}
		openOrders := func() int {
			count := 0
			for _, id := range orders {
				order, getErr := st.GetWorkOrder(ctx, id)
				requireOK(t, getErr)
				if order.State != core.WorkOrderCancelled {
					count++
				}
			}
			return count
		}
		now := time.Now().UTC()
		candidates, err := st.ListElapsedWorkOrderTaskIDs(ctx, now)
		requireOK(t, err)
		selected := 0
		for _, id := range candidates {
			if containsString(tasks, id) {
				selected++
			}
		}
		if selected != store.TerminalOrphanTaskBatch {
			t.Fatalf("one tick selected %d of %d terminal tasks, bound is %d", selected, total, store.TerminalOrphanTaskBatch)
		}
		requireOK(t, firstErr(ops.TickOrderClock(ctx, now)))
		if remaining := openOrders(); remaining != total-store.TerminalOrphanTaskBatch {
			t.Fatalf("after one tick %d orphan orders remain open, want %d", remaining, total-store.TerminalOrphanTaskBatch)
		}
		requireOK(t, firstErr(ops.TickOrderClock(ctx, now)))
		if remaining := openOrders(); remaining != 0 {
			t.Fatalf("after two ticks %d orphan orders remain open", remaining)
		}
	})
}

func newRetirementTask(t *testing.T, x Fixture, state core.TaskState, stage core.Stage) string {
	t.Helper()
	taskID := "terminal-" + core.NewTaskID()
	requireOK(t, x.Backend.CreateTask(x.Context, core.Task{
		ID: taskID, Workspace: x.Workspace, Title: taskID, Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + taskID, State: state, NextStage: stage, CreatedAt: time.Now().UTC(),
	}))
	return taskID
}

func addRetirementOrder(t *testing.T, x Fixture, taskID, id string, stage core.Stage, round, seat int) string {
	t.Helper()
	requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(x.Context, x.Backend, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, ReviewRound: round, ReviewSeat: seat}))
	return id
}

func moveRetirementOrder(t *testing.T, x Fixture, id string, state core.WorkOrderState, command core.WorkOrderCommand) {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, id)
	requireOK(t, err)
	order.State = state
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, command))
}

func completeRetirementOrder(t *testing.T, x Fixture, id string) {
	t.Helper()
	order, err := For(x.Backend).ClaimWorkOrder(x.Context, id, core.WorkOrderClaim{SessionID: id + "-session", ClientToken: id + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	order.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdSubmitSpec))
}

// requireTerminalRetirement asserts the task holds no non-terminal order, each
// retired order was cancelled once by the terminal sweep, and each preserved
// order is still completed.
func requireTerminalRetirement(t *testing.T, x Fixture, taskID string, command core.TaskCommand, retired, preserved []string) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	orders, err := st.ListTaskWorkOrders(ctx, taskID)
	requireOK(t, err)
	for _, order := range orders {
		if order.State != core.WorkOrderCompleted && order.State != core.WorkOrderCancelled {
			t.Fatalf("task %s still holds %s order %s", taskID, order.State, order.ID)
		}
	}
	jobs, err := st.ListJobs(ctx, taskID)
	requireOK(t, err)
	jobState := map[string]core.JobState{}
	for _, job := range jobs {
		jobState[job.ID] = job.State
	}
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	for _, id := range retired {
		order, getErr := st.GetWorkOrder(ctx, id)
		requireOK(t, getErr)
		if order.State != core.WorkOrderCancelled || !order.RetrySuppressed || order.RetrySuppressionReason != core.TaskTerminalSuppressionReason || order.Claimable {
			t.Fatalf("retired order %s=%+v", id, order)
		}
		if jobState[order.JobID] != core.JobFailed {
			t.Fatalf("retired order %s job state=%s, want failed", id, jobState[order.JobID])
		}
		count := 0
		for _, event := range events {
			if event.Kind != "work_order.cancelled" || event.JobID != order.JobID {
				continue
			}
			var payload struct {
				ID          string              `json:"id"`
				State       core.WorkOrderState `json:"state"`
				Command     string              `json:"command"`
				Reason      string              `json:"reason"`
				TaskCommand string              `json:"task_command"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.ID != id {
				continue
			}
			count++
			if payload.State != core.WorkOrderCancelled || payload.Command != string(core.WorkOrderCmdCancel) ||
				payload.Reason != core.TaskTerminalSuppressionReason || payload.TaskCommand != string(command) {
				t.Fatalf("retirement event for %s=%s", id, event.Payload)
			}
		}
		if count != 1 {
			t.Fatalf("order %s has %d terminal work_order.cancelled events, want 1", id, count)
		}
	}
	for _, id := range preserved {
		order, getErr := st.GetWorkOrder(ctx, id)
		requireOK(t, getErr)
		if order.State != core.WorkOrderCompleted || order.RetrySuppressionReason == core.TaskTerminalSuppressionReason {
			t.Fatalf("completed order %s changed: %+v", id, order)
		}
	}
	for _, violation := range store.AuditLifecycleHistory(events) {
		// Seeded orders can lack their creation event; only a recorded prior
		// state is checked against the order.cancel edge.
		if violation.Space == core.WorkOrderLifecycle && violation.From != "" && containsString(retired, violation.Entity) {
			t.Fatalf("lifecycle audit of %s: %+v", taskID, violation)
		}
	}
}

func countRetirementEvents(t *testing.T, x Fixture, taskID string) int {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	count := 0
	for _, event := range events {
		if event.Kind == "work_order.cancelled" {
			count++
		}
	}
	return count
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstErr(_ int, err error) error {
	if err != nil {
		return fmt.Errorf("order clock tick: %w", err)
	}
	return nil
}
