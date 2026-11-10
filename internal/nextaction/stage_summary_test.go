package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestWorkingAndQueuedSummariesNameTheOrderStage pins the stage-specific
// working and queued sentences: the stated order's stage chooses the sentence,
// the task's next stage never does, and an unknown stage or factory work keeps
// the generic sentence (req-operator-next-action AC-1.8, AC-5.1;
// component-next-action invariants 17, 18).
func TestWorkingAndQueuedSummariesNameTheOrderStage(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	working := map[core.Stage]string{
		core.StageSpec:      "An agent is planning this task.",
		core.StageImplement: "An agent is implementing this task.",
		core.StageReview:    "An agent is reviewing this task.",
		core.StageVerify:    "An agent is verifying this task.",
	}
	queued := map[core.Stage]string{
		core.StageSpec:      "The plan order is waiting for an agent.",
		core.StageImplement: "The implementation order is waiting for an agent.",
		core.StageReview:    "The review order is waiting for an agent.",
		core.StageVerify:    "The verification order is waiting for an agent.",
	}
	// The task's next stage disagrees with every order stage on purpose.
	task := core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageMerge}
	for stage, want := range working {
		t.Run("working "+string(stage), func(t *testing.T) {
			got := Project(Input{Task: task, Orders: []core.WorkOrder{{ID: "o", Stage: stage, State: core.WorkOrderClaimed}}, Now: now})
			assertSummary(t, got, ActorAgent, KindWorking, "Working", want)
		})
	}
	for stage, want := range queued {
		t.Run("queued "+string(stage), func(t *testing.T) {
			queuedTask, order := task, core.WorkOrder{ID: "o", Stage: stage, State: core.WorkOrderQueued, Claimable: true}
			if stage == core.StageVerify {
				// A verify order is claimable only at the verify stage of a
				// verify-stage policy, on the reviewed head.
				queuedTask.NextStage, queuedTask.SetupContract.VerifyStage, queuedTask.ReviewedHeadSHA = core.StageVerify, true, "abc"
				order.HeadSHA = "abc"
			}
			got := Project(Input{Task: queuedTask, Orders: []core.WorkOrder{order}, Now: now})
			assertSummary(t, got, ActorAgent, KindQueued, "Queued", want)
		})
	}
	t.Run("working unknown stage", func(t *testing.T) {
		got := Project(Input{Task: core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderClaimed}}, Now: now})
		assertSummary(t, got, ActorAgent, KindWorking, "Working", "An agent is working on this task.")
	})
	t.Run("queued unknown stage", func(t *testing.T) {
		got := Project(Input{Task: core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement}, Orders: []core.WorkOrder{{ID: "o", Stage: core.StageMonitor, State: core.WorkOrderQueued, Claimable: true}}, Now: now})
		assertSummary(t, got, ActorAgent, KindQueued, "Queued", "An order is waiting to be claimed.")
	})
	t.Run("factory working without order", func(t *testing.T) {
		got := Project(Input{Task: core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement}, Now: now})
		assertSummary(t, got, ActorFactory, KindWorking, "Working", "An agent is working on this task.")
	})
	t.Run("factory working with a stated order", func(t *testing.T) {
		// A queued order that is not claimable yet is the factory row's
		// stated order; its stage must not reach the agent sentence.
		got := Project(Input{Task: core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageReview}, Orders: []core.WorkOrder{{ID: "o", Stage: core.StageImplement, State: core.WorkOrderQueued}}, Now: now})
		if got.CurrentOrder == nil || got.CurrentOrder.ID != "o" {
			t.Fatalf("current order=%v want o", got.CurrentOrder)
		}
		assertSummary(t, got, ActorFactory, KindWorking, "Working", "An agent is working on this task.")
	})
}

func assertSummary(t *testing.T, got NextAction, actor, kind, label, summary string) {
	t.Helper()
	if got.Actor != actor || got.Kind != kind || got.Label != label {
		t.Fatalf("actor/kind/label=%s/%s/%q want %s/%s/%q", got.Actor, got.Kind, got.Label, actor, kind, label)
	}
	if got.Summary != summary {
		t.Fatalf("summary=%q want %q", got.Summary, summary)
	}
	if compact := CompactFrom(got); compact.Summary != summary {
		t.Fatalf("compact summary=%q want %q", compact.Summary, summary)
	}
	entry, _ := Lookup(kind, "", "", "")
	if render(entry.IfIgnored, Vars{}) != got.IfIgnored {
		t.Fatalf("if_ignored=%q changed", got.IfIgnored)
	}
}
