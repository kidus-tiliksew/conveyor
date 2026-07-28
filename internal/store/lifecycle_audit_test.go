package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestAuditLifecycleHistoryReportsOnlyEdgesOutsideCanonicalTables(t *testing.T) {
	now := time.Now().UTC()
	events := []core.Event{
		{ID: 1, TaskID: "legal", Kind: "task.state_changed", At: now, Payload: core.JSONPayload(map[string]any{"from": core.TaskClaiming, "to": core.TaskQueued})},
		{ID: 2, TaskID: "legal", Kind: "task.state_changed", At: now.Add(time.Second), Payload: core.JSONPayload(map[string]any{"from": core.TaskQueued, "to": core.TaskRunning, "command": core.TaskDispatchStart})},
		{ID: 3, TaskID: "illegal", Kind: "task.state_changed", At: now.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"from": core.TaskQueued, "to": core.TaskApproved})},
		{ID: 4, TaskID: "orders", JobID: "order-1", Kind: "work_order.created", At: now, Payload: core.JSONPayload(core.WorkOrder{ID: "order-1", State: core.WorkOrderQueued})},
		{ID: 5, TaskID: "orders", JobID: "order-1", Kind: "work_order.updated", At: now.Add(time.Second), Payload: core.JSONPayload(core.WorkOrder{ID: "order-1", State: core.WorkOrderCompleted})},
	}
	violations := AuditLifecycleHistory(events)
	if len(violations) != 2 {
		t.Fatalf("violations = %#v", violations)
	}
	byEntity := map[string]LifecycleAuditViolation{}
	for _, violation := range violations {
		byEntity[violation.Entity] = violation
	}
	if byEntity["illegal"].EventID != 3 {
		t.Fatalf("task violation = %#v", byEntity["illegal"])
	}
	if byEntity["order-1"].EventID != 5 {
		t.Fatalf("order violation = %#v", byEntity["order-1"])
	}
	if events[2].ID != 3 || events[4].ID != 5 {
		t.Fatal("audit modified source history")
	}
}

func TestAuditLifecycleHistoryAcceptsCommandedCancellation(t *testing.T) {
	events := []core.Event{{ID: 1, TaskID: "cancel", Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": core.TaskRunning, "to": core.TaskClosed, "command": core.TaskCancel})}}
	if violations := AuditLifecycleHistory(events); len(violations) != 0 {
		t.Fatalf("violations = %#v", violations)
	}
}

func TestAuditLifecycleHistoryDetectsDisconnectedTaskEdges(t *testing.T) {
	events := []core.Event{
		{ID: 1, TaskID: "task", Kind: "task.created", Payload: core.JSONPayload(core.Task{ID: "task", State: core.TaskQueued})},
		{ID: 2, TaskID: "task", Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": core.TaskQueued, "to": core.TaskRunning, "command": core.TaskDispatchStart})},
		{ID: 3, TaskID: "task", Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": core.TaskClaiming, "to": core.TaskQueued, "command": core.TaskIntakeFinalize})},
	}
	violations := AuditLifecycleHistory(events)
	if len(violations) != 1 || violations[0].EventID != 3 || violations[0].Reason == "" {
		t.Fatalf("violations = %#v", violations)
	}
}

func TestAuditLifecycleHistoryClassifiesHistoricalQueuedRecoveryAsRedispatch(t *testing.T) {
	events := []core.Event{
		{ID: 1, TaskID: "task", JobID: "order", Kind: "work_order.created", Payload: core.JSONPayload(core.WorkOrder{ID: "order", State: core.WorkOrderQueued})},
		{ID: 2, TaskID: "task", JobID: "order", Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{ID: "order", State: core.WorkOrderClaimed})},
		{ID: 3, TaskID: "task", JobID: "order", Kind: "work_order.child_failed", Payload: core.JSONPayload(map[string]any{"outcome": core.WorkOrderOutcomeChildFailure})},
		{ID: 4, TaskID: "task", JobID: "order", Kind: "work_order.recovered", Payload: core.JSONPayload(map[string]any{"prior_state": core.WorkOrderQueued, "new_state": core.WorkOrderQueued})},
	}
	if violations := AuditLifecycleHistory(events); len(violations) != 0 {
		t.Fatalf("violations = %#v", violations)
	}
}
