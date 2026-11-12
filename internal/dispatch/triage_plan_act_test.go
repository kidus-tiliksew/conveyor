package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// blockedTriageAgent holds the triage model call open until the test
// releases it, so a plan act can commit while triage runs.
type blockedTriageAgent struct {
	started chan struct{}
	release chan struct{}
}

func (agent *blockedTriageAgent) Run(ctx context.Context, _ string, _ inprocess.Input) (inprocess.Result, error) {
	close(agent.started)
	select {
	case <-agent.release:
	case <-ctx.Done():
		return inprocess.Result{}, ctx.Err()
	}
	return inprocess.Result{Output: "```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Ready.\"}\n```", TokensIn: 20, TokensOut: 10}, nil
}

// TestTriageProceedUsesThePlanStateCommittedDuringTriage blocks the triage
// model call, commits a plan act while it is blocked, then releases the
// proceed verdict. The proceed step follows the plan state the store reads
// under the task-operation lock, not the snapshot triage started from, and
// the task ends with exactly the work the new state calls for: one open order
// of the right stage, or none at the plan approval gate (component-plan-state
// invariant 5; component-triage invariant 29; req-task-plan-state AC-1.1,
// AC-3.1, AC-3.2).
func TestTriageProceedUsesThePlanStateCommittedDuringTriage(t *testing.T) {
	t.Parallel()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	needed, notRequired := core.PlanNeeded, core.PlanNotRequired
	for _, tc := range []struct {
		name    string
		initial core.PlanState
		act     func(ctx context.Context, st store.Store, taskID string) error
		state   core.TaskState
		next    core.Stage
		gate    core.GateKind
		plan    core.PlanState
	}{
		{name: "attach on not_required enters the plan approval gate", initial: core.PlanNotRequired,
			act: func(ctx context.Context, st store.Store, taskID string) error {
				_, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
					return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: taskID, RequestID: taskID + "-attach", Reason: "operator plan", Plan: routingPlan})
				})
				return err
			},
			state: core.TaskAwaiting, gate: core.GateKindPlanApproval, plan: core.PlanAwaitingApproval},
		{name: "attach on needed enters the plan approval gate", initial: core.PlanNeeded,
			act: func(ctx context.Context, st store.Store, taskID string) error {
				_, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
					return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: taskID, RequestID: taskID + "-attach", Reason: "operator plan", Plan: routingPlan})
				})
				return err
			},
			state: core.TaskAwaiting, gate: core.GateKindPlanApproval, plan: core.PlanAwaitingApproval},
		{name: "policy to needed routes to the plan stage", initial: core.PlanNotRequired,
			act: func(ctx context.Context, st store.Store, taskID string) error {
				_, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
					return st.ChangeTaskPolicyCommand(ctx, lease, store.SetupChangeRequest{TaskID: taskID, RequestID: taskID + "-policy", Reason: "needs design", Policy: &store.TaskPolicyChange{PlanState: &needed}})
				})
				return err
			},
			state: core.TaskQueued, next: core.StageSpec, plan: core.PlanNeeded},
		{name: "policy to not_required routes to implement", initial: core.PlanNeeded,
			act: func(ctx context.Context, st store.Store, taskID string) error {
				_, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
					return st.ChangeTaskPolicyCommand(ctx, lease, store.SetupChangeRequest{TaskID: taskID, RequestID: taskID + "-policy", Reason: "small change", Policy: &store.TaskPolicyChange{PlanState: &notRequired}})
				})
				return err
			},
			state: core.TaskQueued, next: core.StageImplement, plan: core.PlanNotRequired},
		{name: "no act keeps not_required on implement", initial: core.PlanNotRequired, state: core.TaskQueued, next: core.StageImplement, plan: core.PlanNotRequired},
		{name: "no act keeps needed on the plan stage", initial: core.PlanNeeded, state: core.TaskQueued, next: core.StageSpec, plan: core.PlanNeeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "user:routing-operator", Role: core.ActorUser}), "demo")
			st := store.NewMemory()
			taskID := "blocked-triage-" + string(tc.initial) + "-" + core.NewTaskID()
			if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "api", Title: "Route", Body: "Route by plan state.", BaseBranch: "main",
				Branch: "conveyor/task-" + taskID, PolicyVersion: 1, PlanState: tc.initial, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Triage: config.TriageRoute{Model: "triage", Timeout: time.Minute}, Stages: map[string]config.StageRoute{
				"spec":      {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
				"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			}}}
			agent := &blockedTriageAgent{started: make(chan struct{}), release: make(chan struct{})}
			dispatcher := New(st, cfg, agent)
			dispatcher.Pack = bundle
			dispatcher.DisableMemoryQueueForTest()
			done := make(chan error, 1)
			go func() { done <- dispatcher.DispatchNow(ctx, taskID) }()
			select {
			case <-agent.started:
			case <-time.After(10 * time.Second):
				t.Fatal("triage never reached the model call")
			}
			if tc.act != nil {
				if err := tc.act(ctx, st, taskID); err != nil {
					close(agent.release)
					t.Fatalf("plan act during triage: %v", err)
				}
				during, err := st.GetTask(ctx, taskID)
				if err != nil || during.NextStage != core.StageTriage || during.PlanState != tc.plan {
					close(agent.release)
					t.Fatalf("task during triage = %+v err=%v; the act must move the plan state alone", during, err)
				}
			}
			close(agent.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("triage dispatch: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("triage dispatch did not finish")
			}
			current, err := st.GetTask(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != tc.state || current.GateKind != tc.gate || current.PlanState != tc.plan || (tc.state == core.TaskQueued && current.NextStage != tc.next) {
				t.Fatalf("after triage task state=%s next=%s gate=%s plan=%s, want %s next=%s gate=%s plan=%s",
					current.State, current.NextStage, current.GateKind, current.PlanState, tc.state, tc.next, tc.gate, tc.plan)
			}
			if tc.gate == core.GateKindPlanApproval && current.RecoveryStage != core.StageImplement {
				t.Fatalf("plan gate recovery stage=%s, want implement", current.RecoveryStage)
			}
			// Lost-work check: dispatching the task again leaves exactly one
			// open order of the stage the new state chose, or none at the gate.
			if err = dispatcher.DispatchNow(ctx, taskID); err != nil {
				t.Fatalf("dispatch after triage: %v", err)
			}
			if err = dispatcher.DispatchNow(ctx, taskID); err != nil {
				t.Fatalf("second dispatch after triage: %v", err)
			}
			orders, err := st.ListTaskWorkOrders(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			var open []core.WorkOrder
			for _, order := range orders {
				if order.State != core.WorkOrderCancelled && order.State != core.WorkOrderCompleted {
					open = append(open, order)
				}
			}
			switch {
			case tc.gate == core.GateKindPlanApproval:
				if len(open) != 0 {
					t.Fatalf("the plan approval gate holds open orders %+v", open)
				}
			case len(open) != 1 || open[0].Stage != tc.next || open[0].State != core.WorkOrderQueued:
				t.Fatalf("open orders after triage = %+v, want one queued %s order", open, tc.next)
			}
		})
	}
}
