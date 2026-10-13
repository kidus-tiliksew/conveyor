// Package stalemintfixture drives the real dispatcher against a backend to
// prove the stage-mint predicate and the atomic stage-creation guards agree:
// a historical order behind a newer stage never blocks a bounce-back mint, and
// a recoverable stop the clock will not move stays an operator stop with no
// successor (core.WorkOrderBlocksStageMint; component-work-orders "Stages and
// order minting"; component-durable-queue; DEC-62).
package stalemintfixture

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Run executes every case against a fresh backend from factory.
func Run(t *testing.T, factory func(*testing.T) store.Backend) {
	t.Helper()
	t.Run("BounceBehindReviewQueuedImplement", func(t *testing.T) { runBounce(t, factory(t), false) })
	t.Run("BounceBehindReviewStaleImplement", func(t *testing.T) { runBounce(t, factory(t), true) })
	t.Run("RecoveryDisabledStaleHoldsStage", func(t *testing.T) { runRecoveryDisabled(t, factory(t)) })
	t.Run("PolicyRefusedVerifyHoldsStage", func(t *testing.T) { runRefusedVerify(t, factory(t)) })
	t.Run("ClaimedOrderBehindLaterStageKeepsExclusivity", func(t *testing.T) { runClaimedExclusivity(t, factory(t)) })
}

type harness struct {
	ctx  context.Context
	st   store.Backend
	d    *dispatch.Dispatcher
	task core.Task
}

func newHarness(t *testing.T, st store.Backend, name string, recovery config.RecoveryPolicy, task core.Task) harness {
	t.Helper()
	workspace := "stalemint-" + core.NewTaskID()
	settings := config.ContextualExecutionSettings{
		ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
		Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "45m"},
		Verify:         config.ImplementationSettings{Harness: "codex", Model: "gpt", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "1h"},
	}
	h := config.Harness{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}
	setup := config.ExecutionSetup{Name: "backend", ExecutionSettings: settings, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Harness: "codex"}}}}
	if task.SetupContract.VerifyStage {
		setup.VerifyStage = true
	}
	cfg := (&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://github.com/fixture/conveyor", GitHub: "fixture/conveyor", Base: "main"}}, WorkOrderQueueTimeout: time.Hour, WorkOrderQueueTimeoutText: "1h", MaxBounces: 3, Recovery: recovery, Harnesses: []config.Harness{h}, Setups: []config.ExecutionSetup{setup}, DefaultSetup: "backend"}).WithSetup(setup)
	human := store.WithActor(t.Context(), store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err := st.CreateWorkspace(human, workspace, "Stale mint "+workspace, cfg); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	task.ID = name + "-" + core.NewTaskID()
	task.Repo, task.BaseBranch, task.Branch = "conveyor", "main", "conveyor/task-"+task.ID
	task.Workspace, task.Title, task.SetupName, task.SetupContract, task.CreatedAt = workspace, "Stale mint", setup.Name, setup, time.Now().UTC()
	if task.State == "" {
		task.State = core.TaskQueued
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return harness{ctx: ctx, st: st, d: dispatch.New(st, cfg, nil), task: task}
}

func (h harness) dispatch(t *testing.T) {
	t.Helper()
	if err := h.d.DispatchNow(h.ctx, h.task.ID); err != nil {
		t.Fatal(err)
	}
}

func (h harness) perform(t *testing.T, command taskops.Command) {
	t.Helper()
	if _, err := taskops.New(h.st).Perform(h.ctx, h.task.ID, command); err != nil {
		t.Fatalf("%s: %v", command.Kind, err)
	}
}

// running moves a queued task to running so a stage command applies; the
// dispatcher may already have done so when it minted.
func (h harness) running(t *testing.T) {
	t.Helper()
	task, err := h.st.GetTask(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == core.TaskQueued {
		h.perform(t, taskops.Command{Kind: core.TaskDispatchStart})
	}
}

func (h harness) orders(t *testing.T) map[string]core.WorkOrder {
	t.Helper()
	orders, err := h.st.ListTaskWorkOrders(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]core.WorkOrder, len(orders))
	for _, order := range orders {
		byID[order.ID] = order
	}
	return byID
}

func (h harness) expire(t *testing.T, id string) {
	t.Helper()
	order, err := h.st.GetWorkOrder(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	order.QueueEnteredAt = time.Now().UTC().Add(-2 * time.Hour)
	order.QueueDeadline = time.Now().UTC().Add(-time.Second)
	if err = storetest.UpdateWorkOrder(h.ctx, h.st, order); err != nil {
		t.Fatal(err)
	}
}

// runBounce: implement-1 stays stored queued (or reads stale) while the task
// advances to review and a real review round is minted; the review bounces the
// task back. The predicate sees the newer review order as latest, so dispatch
// must mint implement-2, and creation must retire implement-1 rather than
// refuse because a same-stage queued order exists.
func runBounce(t *testing.T, st store.Backend, stale bool) {
	h := newHarness(t, st, "bounce", config.DefaultRecoveryPolicy(), core.Task{NextStage: core.StageImplement})
	first := h.task.ID + "-implement-1"
	h.dispatch(t)
	if stale {
		h.expire(t, first)
	}
	h.running(t)
	h.perform(t, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	h.dispatch(t)
	if _, ok := h.orders(t)[h.task.ID+"-review-1-seat-1"]; !ok {
		t.Fatalf("review round not minted: %+v", h.orders(t))
	}
	h.running(t)
	h.perform(t, taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true})
	h.dispatch(t)
	orders := h.orders(t)
	fresh, ok := orders[h.task.ID+"-implement-2"]
	if !ok || fresh.State != core.WorkOrderQueued {
		summary := map[string]core.WorkOrderState{}
		for id, order := range orders {
			summary[id] = order.State
		}
		task, _ := h.st.GetTask(h.ctx, h.task.ID)
		t.Fatalf("bounce did not mint implement-2: task=%s/%s orders=%v", task.State, task.NextStage, summary)
	}
	if old := orders[first]; old.State != core.WorkOrderCancelled {
		t.Fatalf("implement-1 was not retired: %+v", old)
	}
	// Redelivery after the mint is idempotent.
	h.dispatch(t)
	if _, extra := h.orders(t)[h.task.ID+"-implement-3"]; extra {
		t.Fatal("redelivered dispatch minted implement-3")
	}
}

// requireStop asserts the single order is still the stage holder, the clock
// scheduled no recovery, and redelivered dispatch minted nothing.
func (h harness) requireStop(t *testing.T, id string, want core.WorkOrderState) {
	t.Helper()
	clock := taskops.New(h.st)
	for range 3 {
		if _, err := clock.TickOrderClock(h.ctx, time.Now().UTC().Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		h.dispatch(t)
	}
	orders := h.orders(t)
	if len(orders) != 1 {
		t.Fatalf("operator stop minted a successor: %+v", orders)
	}
	order := orders[id]
	if order.State != want || !order.NextAutomaticRecoveryAt.IsZero() || order.AutomaticRecoveryCount != 0 {
		t.Fatalf("operator stop order=%+v", order)
	}
}

// A claimed same-stage order keeps executor exclusivity even when a later
// order of another stage is the task's latest: creation refuses the successor.
func runClaimedExclusivity(t *testing.T, st store.Backend) {
	h := newHarness(t, st, "claimed", config.DefaultRecoveryPolicy(), core.Task{NextStage: core.StageImplement})
	first := h.task.ID + "-implement-1"
	h.dispatch(t)
	if _, err := storetest.ClaimWorkOrder(h.ctx, h.st, first, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "claimed", ClientToken: "claimed-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	later := h.task.ID + "-review-1-seat-1"
	if err := h.st.CreateJob(h.ctx, core.Job{ID: later, TaskID: h.task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(h.ctx, h.st, core.WorkOrder{ID: later, TaskID: h.task.ID, JobID: later, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	next := h.task.ID + "-implement-2"
	created, err := taskops.ExecuteWorkOrder(h.ctx, h.st, h.task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) {
		return h.st.CreateStageWorkOrderCommand(h.ctx, lease, core.Job{ID: next, TaskID: h.task.ID, Stage: core.StageImplement, State: core.JobPending},
			core.WorkOrder{ID: next, TaskID: h.task.ID, JobID: next, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
	})
	if err != nil || created {
		t.Fatalf("successor beside a live claim: created=%v err=%v", created, err)
	}
	if order := h.orders(t)[first]; order.State != core.WorkOrderClaimed {
		t.Fatalf("claimed order disturbed: %+v", order)
	}
}

// R1: with automatic recovery disabled a stale order still holds the stage.
func runRecoveryDisabled(t *testing.T, st store.Backend) {
	h := newHarness(t, st, "disabled", config.RecoveryPolicy{AutomaticRecoveries: 0, WindowText: "24h"}, core.Task{NextStage: core.StageImplement})
	first := h.task.ID + "-implement-1"
	h.dispatch(t)
	h.expire(t, first)
	h.requireStop(t, first, core.WorkOrderStale)
}

// A stale verify order whose head no longer matches the verify head is
// refused by recovery policy; it still holds the stage for the operator.
func runRefusedVerify(t *testing.T, st store.Backend) {
	h := newHarness(t, st, "verify", config.DefaultRecoveryPolicy(), core.Task{NextStage: core.StageVerify, ReviewedHeadSHA: "head-new", SetupContract: config.ExecutionSetup{VerifyStage: true}})
	id := h.task.ID + "-verify-1"
	now := time.Now().UTC()
	if err := h.st.CreateJob(h.ctx, core.Job{ID: id, TaskID: h.task.ID, Stage: core.StageVerify, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(h.ctx, h.st, core.WorkOrder{ID: id, TaskID: h.task.ID, JobID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head-old", QueueEnteredAt: now.Add(-2 * time.Hour), QueueDeadline: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if refusal := store.WorkOrderRecoveryRefusal(h.task, core.WorkOrder{ID: id, TaskID: h.task.ID, Stage: core.StageVerify, HeadSHA: "head-old"}, nil); refusal == nil {
		t.Fatal("fixture verify order is not policy-refused")
	}
	h.requireStop(t, id, core.WorkOrderStale)
}
