package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestStallTimedOutAndFutureRecovery(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	hit := Stall(core.Task{State: core.TaskRunning}, []core.WorkOrder{{ID: "o", State: core.WorkOrderTimedOut}}, nil, nil, now)
	if hit == nil || hit.Reason != StallTimedOut {
		t.Fatalf("timed out stall=%+v", hit)
	}
	skipped := Stall(core.Task{State: core.TaskRunning}, []core.WorkOrder{{ID: "o", State: core.WorkOrderTimedOut}}, map[string]AutomaticRecovery{"o": {NextAt: &later}}, nil, now)
	if skipped != nil {
		t.Fatalf("future recovery still stalled: %+v", skipped)
	}
}

func TestVerifyDispatchFallback(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	task := core.Task{State: core.TaskRunning}
	order := core.WorkOrder{ID: "v", Stage: core.StageVerify, State: core.WorkOrderQueued}
	if err := core.ValidateVerifyDispatch(task, order); err == nil {
		t.Fatal("fixture no longer refuses verify dispatch")
	}
	hit := Stall(task, []core.WorkOrder{order}, nil, nil, now)
	if hit == nil || hit.Reason != StallUnclaimable {
		t.Fatalf("verify fallback=%+v", hit)
	}
}
