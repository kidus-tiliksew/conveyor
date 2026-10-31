package store_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Row 10 asks for a verification disposition only while a verify order is
// queued and released at its sealed checkpoint, and then always names that
// verify order, even when a later review order waits unclaimed. A verify
// order that was recovered and has moved on keeps its checkpoint for history;
// that historical checkpoint never selects row 10, so the task is never left
// at a disposition whose applied and not-applied choices are both refused
// with no order to recover (feature-operator-next-action NA-3 row 10; task
// 261030-54fbd3).
func TestVerificationDispositionNamesTheOrderAtTheCheckpoint(t *testing.T) {
	f := newBatchFixture(t)
	sealed := func(task core.Task) func(*core.WorkOrder) {
		return func(order *core.WorkOrder) {
			order.LastAttemptID, order.LastAttemptOutcome = "attempt-"+task.ID, core.WorkOrderOutcomeReleased
			order.RetrySuppressed, order.RetrySuppressionReason = true, core.WorkOrderReleaseReasonOperatorCheckpointReached
			order.Checkpoint = &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "context-" + task.ID, HeadSHA: "head", Reason: "unresolved", RequiredAction: "dispose", OperationIDs: []string{"operation-" + task.ID}}}
		}
	}
	// moved records the state a recovered verify order reaches after the
	// fixture creates it queued: an operator recovery keeps the checkpoint
	// for history and clears the released outcome, and the order may then be
	// claimed and submitted or completed. The read takes these shapes as its
	// loaded orders, because the store creates every order queued.
	moved := map[string]core.WorkOrderState{}
	recoveredThen := func(task core.Task, state core.WorkOrderState) func(*core.WorkOrder) {
		return func(order *core.WorkOrder) {
			sealed(task)(order)
			order.LastAttemptOutcome, order.RetrySuppressed, order.RetrySuppressionReason = "", false, ""
			order.RedispatchCount = 1
			moved[order.ID] = state
		}
	}
	unclaimedReview := func(order *core.WorkOrder) { order.Claimable = true }

	type shape struct {
		task        core.Task
		disposition bool
		verify      string
	}
	shapes := map[string]*shape{}
	add := func(name string, disposition bool, build func(task core.Task) string) {
		task := f.task(t, name, core.TaskRunning, "")
		shapes[name] = &shape{task: task, disposition: disposition, verify: build(task)}
	}
	add("at checkpoint", true, func(task core.Task) string {
		return f.order(t, task, core.StageVerify, 1, sealed(task)).ID
	})
	add("at checkpoint with a later unclaimed review", true, func(task core.Task) string {
		verify := f.order(t, task, core.StageVerify, 1, sealed(task))
		f.order(t, task, core.StageReview, 1, unclaimedReview)
		return verify.ID
	})
	add("recovered and requeued", false, func(task core.Task) string {
		return f.order(t, task, core.StageVerify, 1, recoveredThen(task, core.WorkOrderQueued)).ID
	})
	add("recovered and submitted with a later unclaimed review", false, func(task core.Task) string {
		verify := f.order(t, task, core.StageVerify, 1, recoveredThen(task, core.WorkOrderSubmitted))
		f.order(t, task, core.StageReview, 1, unclaimedReview)
		return verify.ID
	})
	add("recovered and completed with a later unclaimed review", false, func(task core.Task) string {
		verify := f.order(t, task, core.StageVerify, 1, recoveredThen(task, core.WorkOrderCompleted))
		f.order(t, task, core.StageReview, 1, unclaimedReview)
		return verify.ID
	})

	tasks := make([]core.Task, 0, len(shapes))
	for _, s := range shapes {
		stored, err := f.st.GetTask(f.ctx, s.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, stored)
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	orders, err := f.st.ListWorkOrdersForTasks(f.ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for i := range orders {
		if state, ok := moved[orders[i].ID]; ok {
			orders[i].State = state
		}
	}
	got, err := store.ReadNextActions(f.ctx, f.st, tasks, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities, OrdersLoaded: true, Orders: orders})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		s := shapes[name]
		action := got[s.task.ID]
		for _, refusal := range action.Details.Refusals {
			if refusal.Text == "no order to recover" {
				t.Errorf("%s: %s refused with no order to recover", name, refusal.Choice)
			}
		}
		if !s.disposition {
			if action.Kind == nextaction.KindVerificationDisposition {
				t.Errorf("%s: kind = %s from a checkpoint kept for history", name, action.Kind)
			}
			for _, secondary := range action.Details.Secondary {
				if secondary.Kind == nextaction.KindVerificationDisposition {
					t.Errorf("%s: secondary %s from a checkpoint kept for history", name, secondary.Kind)
				}
			}
			continue
		}
		if action.Kind != nextaction.KindVerificationDisposition || action.CurrentOrder == nil || action.CurrentOrder.ID != s.verify {
			t.Errorf("%s: next action = %s on %+v, want verification_disposition on %s", name, action.Kind, action.CurrentOrder, s.verify)
		}
	}
}
