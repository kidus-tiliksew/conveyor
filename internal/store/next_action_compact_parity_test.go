package store_test

import (
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// A compact read states the same compact fields as a full read of the same
// task. For a verify order released at its sealed verification checkpoint
// with a decision request, the disposition set decides between row 10 and
// row 11, so a compact read reads the set of that one order. It reads no
// verification record for any other task, including a sealed order without a
// decision request, where the set changes no compact field
// (component-next-action invariant 15).
func TestCompactReadReadsTheSetOnlyWhereItDecidesTheRow(t *testing.T) {
	f := newBatchFixture(t)
	f.add(t, false)
	sealed := f.task(t, "sealed question", core.TaskRunning, "")
	f.history(t, sealed.ID, false)
	f.order(t, sealed, core.StageVerify, 1, func(order *core.WorkOrder) {
		// The seal writes a decision request beside the verification
		// checkpoint. The fixture's context has no verification records, so
		// the set is refused.
		order.LastAttemptID, order.LastAttemptOutcome = "attempt-"+sealed.ID, core.WorkOrderOutcomeReleased
		order.RetrySuppressed, order.RetrySuppressionReason = true, core.WorkOrderReleaseReasonOperatorCheckpointReached
		order.Checkpoint = &core.WorkOrderCheckpoint{
			DecisionRequest: "Provider acknowledgement is unknown\nRequired operator action: Inspect the provider",
			Verification:    &core.WorkOrderVerificationCheckpoint{ContextID: "context-" + sealed.ID, HeadSHA: "head", Reason: "unresolved", RequiredAction: "dispose", OperationIDs: []string{"operation-" + sealed.ID}},
		}
	})
	stored, err := f.st.GetTask(f.ctx, sealed.ID)
	if err != nil {
		t.Fatal(err)
	}
	tasks := append(append([]core.Task(nil), f.requested...), stored)

	counter := newCountingStore(f.st)
	compact, err := store.ReadNextActions(f.ctx, counter, tasks, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities, CompactOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	counter.mu.Lock()
	reads := counter.args["ReadVerificationReviews"]
	counter.mu.Unlock()
	if len(reads) != 1 || !reflect.DeepEqual(reads[0], []string{sealed.ID}) {
		t.Fatalf("compact verification reads = %v, want one read of %s only", reads, sealed.ID)
	}
	if calls := counter.reset(); calls["ReadVerificationReview"] != 0 {
		t.Fatalf("compact read issued per-task verification reads: %v", calls)
	}

	full, err := store.ReadNextActions(f.ctx, f.st, tasks, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if got, want := nextaction.CompactFrom(compact[task.ID]), nextaction.CompactFrom(full[task.ID]); !reflect.DeepEqual(got, want) {
			t.Fatalf("task %s compact = %+v, full = %+v", task.ID, got, want)
		}
	}
	if full[sealed.ID].Kind != nextaction.KindAgentQuestion || full[sealed.ID].Details.Dispositions != nil {
		t.Fatalf("refused sealed checkpoint = %s dispositions %+v, want agent_question", full[sealed.ID].Kind, full[sealed.ID].Details.Dispositions)
	}
}
