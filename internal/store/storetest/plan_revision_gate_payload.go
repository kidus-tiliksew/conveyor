package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// PlanRevisionGateFunc is dispatch.PendingPlanRevisionGate's pending/error
// result. storetest cannot import dispatch: dispatch integration tests import
// storetest.
type PlanRevisionGateFunc func(context.Context, store.Store, string) (bool, error)

// RunPlanRevisionGatePayloadConformance proves a corrupt plan-revision gate is
// fail-closed on every backend. A completed request always records its request
// event, so the missing-request case appends the gate onto an awaiting task
// that has none.
func RunPlanRevisionGatePayloadConformance(t *testing.T, st store.Store, ctx context.Context, gate PlanRevisionGateFunc) {
	t.Helper()
	if gate == nil {
		t.Fatal("factory must wire dispatch.PendingPlanRevisionGate")
	}
	t.Run("undecodable transition", func(t *testing.T) {
		runUnreadablePlanRevisionGate(t, st, ctx, gate, "transition", func(taskID string, at time.Time) []core.Event {
			return []core.Event{{
				TaskID: taskID, Kind: "task.state_changed", At: at,
				Payload: json.RawMessage(`{"command":7}`),
			}}
		})
	})
	t.Run("undecodable request", func(t *testing.T) {
		runUnreadablePlanRevisionGate(t, st, ctx, gate, "request", func(taskID string, at time.Time) []core.Event {
			return []core.Event{
				{
					TaskID: taskID, Kind: "work_order.plan_revision_requested", At: at,
					Payload: json.RawMessage(`{"work_order_id":1}`),
				},
				{
					TaskID: taskID, Kind: "task.state_changed", At: at.Add(time.Millisecond),
					Payload: core.JSONPayload(map[string]any{"command": core.TaskGatePlanRevision, "from": core.TaskRunning, "to": core.TaskAwaiting}),
				},
			}
		})
	})
	t.Run("missing request", func(t *testing.T) {
		runMissingPlanRevisionRequest(t, st, ctx, gate)
	})
}

func runUnreadablePlanRevisionGate(t *testing.T, st store.Store, ctx context.Context, gate PlanRevisionGateFunc, name string, corrupt func(taskID string, at time.Time) []core.Event) {
	t.Helper()
	fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
	if _, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "the API moved"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	probe := core.WorkOrder{
		ID: fixture.order.TaskID + "-implement-probe-" + name, JobID: fixture.order.TaskID + "-job-probe-" + name,
		TaskID: fixture.order.TaskID, Stage: core.StageImplement, State: core.WorkOrderQueued,
		CreatedAt: now, QueueEnteredAt: now.Add(-2 * time.Hour), QueueDeadline: now.Add(-time.Hour),
	}
	requireOK(t, st.CreateJob(ctx, core.Job{ID: probe.JobID, TaskID: probe.TaskID, Stage: probe.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, probe))
	for _, event := range corrupt(fixture.order.TaskID, now.Add(time.Second)) {
		requireOK(t, st.AppendEvent(ctx, event))
	}
	assertUnreadablePlanRevisionGate(t, st, ctx, gate, probe.ID, fixture.order.TaskID)
}

func runMissingPlanRevisionRequest(t *testing.T, st store.Store, ctx context.Context, gate PlanRevisionGateFunc) {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{
		ID: core.NewTaskID(), Workspace: workspaceID(ctx), Repo: "app", State: core.TaskAwaiting,
		NextStage: core.StageImplement, CreatedAt: now,
	}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, st.CreateTask(ctx, task))
	order := core.WorkOrder{
		ID: task.ID + "-implement-1", JobID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement,
		State: core.WorkOrderQueued, CreatedAt: now, QueueEnteredAt: now.Add(-2 * time.Hour), QueueDeadline: now.Add(-time.Hour),
	}
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, order))
	requireOK(t, st.AppendEvent(ctx, core.Event{
		TaskID: task.ID, Kind: "task.state_changed", At: now,
		Payload: core.JSONPayload(map[string]any{"command": core.TaskGatePlanRevision, "from": core.TaskRunning, "to": core.TaskAwaiting}),
	}))
	assertUnreadablePlanRevisionGate(t, st, ctx, gate, order.ID, task.ID)
}

func assertUnreadablePlanRevisionGate(t *testing.T, st store.Store, ctx context.Context, gate PlanRevisionGateFunc, orderID, taskID string) {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	requireOK(t, err)
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	want := "work order " + orderID + " cannot be recovered while task " + taskID + " has an unreadable plan-revision request; use the plan-revision decision gate"
	if err = store.WorkOrderPlanRevisionRefusal(task, events, orderID); err == nil || err.Error() != want {
		t.Fatalf("refusal err=%v want %q", err, want)
	}
	if _, err = RecoverWorkOrder(ctx, st, orderID, "unreadable-"+orderID, time.Hour); err == nil || err.Error() != want {
		t.Fatalf("recover err=%v want %q", err, want)
	}
	if _, err = RedispatchWorkOrder(ctx, st, orderID, time.Hour); err == nil || err.Error() != want {
		t.Fatalf("redispatch err=%v want %q", err, want)
	}
	pending, err := gate(ctx, st, taskID)
	if !pending || !errors.Is(err, store.ErrPlanRevisionGateUnreadable) {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if !strings.Contains(err.Error(), taskID) {
		t.Fatalf("gate err=%v", err)
	}
}
