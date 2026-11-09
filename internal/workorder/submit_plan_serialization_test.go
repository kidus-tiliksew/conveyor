package workorder

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TestSubmitPlanSerializesWithAnOperatorAttach holds the task side-effect
// lock that the attach route takes, starts submit_plan, and attaches a plan
// while the lock is held. The submission waits for the lock, then finds its
// order cancelled and records nothing, so the attached plan stays the latest
// version at the plan approval gate (component-plan-state invariant 5;
// req-task-plan-state AC-3.2).
func TestSubmitPlanSerializesWithAnOperatorAttach(t *testing.T) {
	t.Parallel()
	env := newPlanSeatsEnv(t, "plan-attach-race", true, frozenSeats(1), 1)
	order := env.claimPlan(t)
	held := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- env.st.WithTaskSideEffectLock(env.ctx, env.taskID, func(locked context.Context) error {
			close(held)
			<-release
			_, err := taskops.ExecuteSetupChange(locked, env.st, env.taskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
				return env.st.AttachTaskPlanCommand(locked, lease, store.AttachPlanRequest{TaskID: env.taskID, RequestID: "attach-during-submit", Reason: "operator plan", Plan: planSeatsMarkdown})
			})
			return err
		})
	}()
	<-held
	submitDone := make(chan error, 1)
	go func() {
		_, err := env.service.SubmitPlan(env.ctx, order.ID, order.SessionID, pipeline.StructuredPlan{Markdown: planSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}})
		submitDone <- err
	}()
	select {
	case err := <-submitDone:
		close(release)
		t.Fatalf("submit_plan finished while the task lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, ok, err := env.st.GetLatestSpecVersion(env.ctx, env.taskID); err != nil || ok {
		close(release)
		t.Fatalf("submit_plan recorded a version while the task lock was held: ok=%t err=%v", ok, err)
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatalf("attach: %v", err)
	}
	select {
	case err := <-submitDone:
		if err == nil {
			t.Fatal("submit_plan succeeded on an order the attach cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("submit_plan did not finish after the lock was released")
	}
	latest, ok, err := env.st.GetLatestSpecVersion(env.ctx, env.taskID)
	if err != nil || !ok || latest.Version != 1 || latest.Origin != core.PlanOriginOperator {
		t.Fatalf("latest plan version = %+v ok=%t err=%v, want the attached version 1", latest, ok, err)
	}
	task, err := env.st.GetTask(env.ctx, env.taskID)
	if err != nil || task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanApproval || task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("task = %+v err=%v, want awaiting_approval at the plan approval gate", task, err)
	}
}
