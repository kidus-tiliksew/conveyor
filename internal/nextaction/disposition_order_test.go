package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// A disposition marker selects row 10 only when a verify order can carry the
// applied and not-applied choices, and row 10 then names the verify order
// released at its verification checkpoint, never a later review order or a
// later verify order. A marker with no queued or claimed verify order behind
// it falls through to the task's other rows instead of offering two refused
// choices (feature-operator-next-action NA-3 row 10; task 261030-54fbd3).
func TestVerificationDispositionNeedsAnOrderToDispose(t *testing.T) {
	now := time.Date(2026, 10, 30, 17, 35, 0, 0, time.UTC)
	checkpoint := &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx", OperationIDs: []string{"op"}}}
	atCheckpoint := core.WorkOrder{ID: "verify-1", Stage: core.StageVerify, State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased, RetrySuppressed: true, Checkpoint: checkpoint, QueueDeadline: now.Add(time.Hour)}
	review := core.WorkOrder{ID: "review-1-seat-1", Stage: core.StageReview, State: core.WorkOrderQueued, Claimable: true, QueueDeadline: now.Add(time.Hour)}
	binding := map[string]DispositionInput{"verify-1": {Binding: &Disposition{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "d"}}}

	for name, orders := range map[string][]core.WorkOrder{
		"verify order alone":                  {atCheckpoint},
		"with a later unclaimed review order": {atCheckpoint, review},
	} {
		in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: orders, Dispositions: binding, Capabilities: allCaps(), Now: now}
		got := Project(in)
		if got.Kind != KindVerificationDisposition || got.CurrentOrder == nil || got.CurrentOrder.ID != "verify-1" {
			t.Fatalf("%s: next action = %s on %+v, want verification_disposition on verify-1", name, got.Kind, got.CurrentOrder)
		}
		for _, id := range []string{"applied", "not_applied"} {
			choice, ok := choiceByID(got, id)
			if !ok || choice.Path != "/v1/work-orders/verify-1/recover" || !choice.Allowed {
				t.Fatalf("%s: %s = %+v (refusals %+v), want an allowed recover of verify-1", name, id, choice, got.Details.Refusals)
			}
		}
	}

	// A later verify order without the checkpoint does not displace the
	// order the binding was read for.
	later := core.WorkOrder{ID: "verify-2", Stage: core.StageVerify, State: core.WorkOrderQueued, QueueDeadline: now.Add(time.Hour)}
	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{atCheckpoint, later}, Dispositions: binding, Capabilities: allCaps(), Now: now}
	if got := Project(in); got.Kind != KindVerificationDisposition || got.CurrentOrder == nil || got.CurrentOrder.ID != "verify-1" {
		t.Fatalf("later verify order: next action = %s on %+v, want verification_disposition on verify-1", got.Kind, got.CurrentOrder)
	}

	// The verify order moved on after recovery; its checkpoint is history.
	for _, state := range []core.WorkOrderState{core.WorkOrderSubmitted, core.WorkOrderCompleted, core.WorkOrderCancelled} {
		done := atCheckpoint
		done.State, done.LastAttemptOutcome, done.RetrySuppressed = state, "", false
		in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{done, review}, Capabilities: allCaps(), Now: now}
		got := Project(in)
		if got.Kind == KindVerificationDisposition || hasSecondary(got, KindVerificationDisposition) {
			t.Fatalf("verify order %s: next action = %s secondary %+v, want no verification_disposition", state, got.Kind, got.Details.Secondary)
		}
		if hasRefusalText(got, "applied", "no order to recover") || hasRefusalText(got, "not_applied", "no order to recover") {
			t.Fatalf("verify order %s: refusals %+v", state, got.Details.Refusals)
		}
	}
}

func TestAtVerificationCheckpoint(t *testing.T) {
	checkpoint := &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx"}}
	base := core.WorkOrder{Stage: core.StageVerify, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeReleased, Checkpoint: checkpoint}
	if !AtVerificationCheckpoint(base) {
		t.Fatal("a queued verify order released at its verification checkpoint is at the checkpoint")
	}
	for name, mutate := range map[string]func(*core.WorkOrder){
		"recovered":           func(o *core.WorkOrder) { o.LastAttemptOutcome = "" },
		"expired":             func(o *core.WorkOrder) { o.LastAttemptOutcome = core.WorkOrderOutcomeExpired },
		"claimed":             func(o *core.WorkOrder) { o.State = core.WorkOrderClaimed },
		"submitted":           func(o *core.WorkOrder) { o.State = core.WorkOrderSubmitted },
		"completed":           func(o *core.WorkOrder) { o.State = core.WorkOrderCompleted },
		"no checkpoint":       func(o *core.WorkOrder) { o.Checkpoint = nil },
		"decision checkpoint": func(o *core.WorkOrder) { o.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm."} },
		"implement stage":     func(o *core.WorkOrder) { o.Stage = core.StageImplement },
	} {
		order := base
		mutate(&order)
		if AtVerificationCheckpoint(order) {
			t.Fatalf("%s: reported at the verification checkpoint", name)
		}
	}
}
