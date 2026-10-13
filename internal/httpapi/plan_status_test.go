package httpapi

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestAwaitingPlanStatusUsesGateKind(t *testing.T) {
	task := core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}
	events := []core.Event{{Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": core.TaskInterventionRedirect})}}
	status := taskPlanStatusFor(task, core.SpecVersion{Version: 1}, true, events)
	if status.State != taskPlanPendingGate {
		t.Fatalf("awaiting gate status=%+v", status)
	}
	closed := taskPlanStatusFor(core.Task{State: core.TaskClosed}, core.SpecVersion{Version: 1}, true, events)
	if closed.State != taskPlanRedirected {
		t.Fatalf("closed status=%+v", closed)
	}
}
