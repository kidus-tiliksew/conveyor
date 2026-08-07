package workorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func setupChangeFixture(t *testing.T, nextSeats []config.ReviewSeat) (*Service, store.Store, context.Context, core.Task, config.ExecutionSetup, config.ExecutionSetup) {
	t.Helper()
	settings := func(harness, model string) config.ContextualExecutionSettings {
		return config.ContextualExecutionSettings{
			ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}},
			Spec:           config.ImplementationSettings{Harness: harness, Model: model, ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "30m"},
			Implementation: config.ImplementationSettings{Harness: harness, Model: model, ModelPolicy: config.ModelPolicyExplicit, Effort: "medium", TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "45m", FallbackHarness: harness},
		}
	}
	harness := func(name string) config.Harness {
		return config.Harness{Name: name, Command: []string{name, "{prompt}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"medium": {"--effort", "medium"}, "high": {"--effort", "high"}}, ProbeCommand: []string{name, "--version"}, ProbeTimeoutText: "5s"}
	}
	old := config.ExecutionSetup{Name: "old", ExecutionSettings: settings("codex", "gpt-old"), Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "old-seat", Harness: "codex"}}}, RefreshReview: config.RefreshReviewDelta}
	next := config.ExecutionSetup{Name: "next", ExecutionSettings: settings("claude", "claude-next"), Review: config.ReviewPanel{Seats: nextSeats}, RefreshReview: config.RefreshReviewDelta}
	cfg := (&config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness("codex"), harness("claude")}, Setups: []config.ExecutionSetup{old, next}, DefaultSetup: old.Name}).WithSetup(old)
	cfg.Setups, cfg.DefaultSetup = []config.ExecutionSetup{old, next}, old.Name
	ctx := store.WithActor(store.WithWorkspace(context.Background(), "demo"), store.Actor{ID: "operator", Role: core.ActorHuman})
	st := store.NewMemory()
	task := core.Task{ID: "setup-change", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, SetupName: old.Name, SetupContract: old, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs, orders, err := dispatch.BuildReviewRound(cfg, task, cfg.WithSetup(old).Routing.Stages["review"], 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	completed, err := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "completed-session", ClientToken: "completed-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	completed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, completed); err != nil {
		t.Fatal(err)
	}
	interrupted := orders[1]
	interrupted.RetrySuppressed, interrupted.LastAttemptOutcome = true, core.WorkOrderOutcomeExpired
	if err = storetest.For(st).UpdateWorkOrder(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}, st, ctx, task, old, next
}

func TestChangeTaskSetupReconcilesInterruptedRoundAndIsIdempotent(t *testing.T) {
	service, st, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude", Effort: "high"}})
	ordersBefore, _ := st.ListTaskWorkOrders(ctx, task.ID)
	var retained core.WorkOrder
	for _, order := range ordersBefore {
		if order.ReviewSeat == 1 {
			retained = order
			break
		}
	}
	if retained.ID == "" {
		t.Fatal("review seat 1 not found")
	}
	if err := storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: retained.JobID, ReviewWorkOrderID: retained.ID,
		Verdict: "approve", ReasonCode: "approved", Summary: "compatible", ReviewedCommitSHA: "head", ReviewRound: 1, ReviewSeat: 1,
		RequiredModel: retained.RequiredModel, RequiredHarness: retained.RequiredHarness, RequiredEffort: retained.RequiredEffort, MaxBounces: 4}); err != nil {
		t.Fatal(err)
	}
	result, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "use corrected routing", "setup-request-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewTransition != "same_round_reconciled" || len(result.RetainedWorkOrders) != 1 || len(result.UpdatedWorkOrders) != 1 || len(result.CreatedWorkOrders) != 0 {
		t.Fatalf("result=%+v", result)
	}
	updated, _ := st.GetWorkOrder(ctx, result.UpdatedWorkOrders[0])
	if updated.RequiredModel != "new-seat" || updated.RequiredHarness != "claude" || updated.RequiredEffort != "high" || updated.RetrySuppressed {
		t.Fatalf("updated=%+v", updated)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, updated.ID, core.WorkOrderClaim{SessionID: "fresh-review", ClientToken: "fresh-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: claimed.JobID, ReviewWorkOrderID: claimed.ID,
		Verdict: "approve", ReasonCode: "approved", Summary: "fresh", ReviewedCommitSHA: "head", ReviewRound: 1, ReviewSeat: 2,
		RequiredModel: claimed.RequiredModel, RequiredHarness: claimed.RequiredHarness, RequiredEffort: claimed.RequiredEffort, MaxBounces: 4}); err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "use corrected routing", "setup-request-1")
	if err != nil || duplicate.Task.SetupName != next.Name {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	if _, err = service.ChangeTaskSetup(ctx, task.ID, next.Name, "different reason", "setup-request-1"); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("conflict=%v", err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	var changes, completed int
	for _, event := range events {
		if event.Kind == "task.setup.changed" {
			changes++
		}
		if event.Kind == "review.round_completed" {
			completed++
		}
	}
	if changes != 1 || completed != 1 {
		t.Fatalf("task.setup.changed=%d review.round_completed=%d", changes, completed)
	}
}

func TestChangeTaskSetupAcceptsOptionalReasonAndRequiresSetupAndRequestID(t *testing.T) {
	t.Run("whitespace reason is normalized and accepted", func(t *testing.T) {
		service, _, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude"}})
		result, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, " \t ", "reasonless-request")
		if err != nil || result.Task.SetupName != next.Name {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	for _, tc := range []struct {
		name      string
		setup     string
		requestID string
	}{
		{name: "blank setup", setup: " \t ", requestID: "valid-request"},
		{name: "blank request id", setup: "next", requestID: " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, ctx, task, _, _ := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude"}})
			if _, err := service.ChangeTaskSetup(ctx, task.ID, tc.setup, "", tc.requestID); err == nil || !strings.Contains(err.Error(), "setup and request_id are required") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestChangeTaskSetupCreatesWholeRoundWhenPanelSizeChanges(t *testing.T) {
	service, st, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "one", Harness: "claude"}, {Model: "two", Harness: "claude"}, {Model: "three", Harness: "claude"}})
	result, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "expand panel", "setup-request-panel")
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewTransition != "new_full_round" || len(result.SupersededWorkOrders) != 2 || len(result.CreatedWorkOrders) != 3 {
		t.Fatalf("result=%+v", result)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	var roundTwo int
	for _, order := range orders {
		if order.ReviewRound == 2 {
			roundTwo++
		}
		if order.ReviewRound == 1 && order.State == core.WorkOrderQueued {
			t.Fatalf("superseded round remained claimable: %+v", order)
		}
	}
	if roundTwo != 3 {
		t.Fatalf("round two seats=%d orders=%+v", roundTwo, orders)
	}
	if cancelled, countErr := st.CountEvents(ctx, task.ID, "work_order.cancelled"); countErr != nil || cancelled != 1 {
		t.Fatalf("canonical setup-change cancellations=%d err=%v", cancelled, countErr)
	}
}

func TestChangeTaskSetupAllowsSubmittedImplementAttempt(t *testing.T) {
	service, st, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude", Effort: "high"}})
	if err := st.CreateJob(ctx, core.Job{ID: "setup-change-implement-1", TaskID: task.ID, Stage: core.StageImplement, Harness: "external-mcp", ModelTier: "gpt-old", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	submitted := core.WorkOrder{ID: "setup-change-implement-1", TaskID: task.ID, JobID: "setup-change-implement-1", Stage: core.StageImplement,
		State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)}
	if err := storetest.For(st).CreateWorkOrder(ctx, submitted); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, submitted.ID, core.WorkOrderClaim{SessionID: "delivered", ClientToken: "delivered-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	result, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "reroute review while held", "setup-submitted")
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.SetupName != next.Name || result.ReviewTransition != "same_round_reconciled" {
		t.Fatalf("result=%+v", result)
	}
	untouched, _ := st.GetWorkOrder(ctx, submitted.ID)
	if untouched.State != core.WorkOrderSubmitted {
		t.Fatalf("delivered attempt mutated=%+v", untouched)
	}
}

func TestChangeTaskSetupRejectsInFlightReviewVerdict(t *testing.T) {
	service, st, ctx, task, old, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude"}})
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	for _, order := range orders {
		if order.Stage == core.StageReview && order.State == core.WorkOrderQueued {
			order.RetrySuppressed, order.LastAttemptOutcome = false, ""
			if err := storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "verdict", ClientToken: "verdict-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			claimed.State = core.WorkOrderSubmitted
			if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "verdict in flight", "setup-verdict"); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.SetupName != old.Name {
		t.Fatalf("task mutated=%+v", current)
	}
}

func TestChangeTaskSetupRejectsClaimedAttemptWithoutMutation(t *testing.T) {
	service, st, ctx, task, old, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude"}})
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	for _, order := range orders {
		if order.State == core.WorkOrderQueued {
			order.RetrySuppressed, order.LastAttemptOutcome = false, ""
			if err := storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "active", ClientToken: "secret", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "too late", "setup-active"); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.SetupName != old.Name {
		t.Fatalf("task mutated=%+v", current)
	}
}

func TestPreemptThenChangeSetupReleaseHoldAndClaimUsesNewContract(t *testing.T) {
	service, st, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{
		{Model: "stable", Harness: "codex"},
		{Model: "new-seat", Harness: "claude", Effort: "high"},
	})
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var target core.WorkOrder
	for _, order := range orders {
		if order.ReviewSeat == 2 {
			target = order
			break
		}
	}
	if target.ID == "" {
		t.Fatal("review seat 2 not found")
	}
	target.RetrySuppressed, target.LastAttemptOutcome = false, ""
	if err = storetest.For(st).UpdateWorkOrder(ctx, target); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, target.ID, core.WorkOrderClaim{
		SessionID: "old-contract-session", ClientToken: "old-contract-token", ClaimantID: "worker-old", WorkerID: "worker-old",
		Agent: "codex", Model: target.RequiredModel, Lease: time.Hour, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetTaskHold(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Preempt(ctx, target.ID, "replace the execution contract", "preempt-before-setup-change"); err != nil {
		t.Fatal(err)
	}
	changed, err := service.ChangeTaskSetup(ctx, task.ID, next.Name, "use the repaired setup", "setup-after-preempt")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Task.SetupName != next.Name {
		t.Fatalf("setup change=%+v", changed)
	}
	if _, err = st.SetTaskHold(ctx, task.ID, false); err != nil {
		t.Fatal(err)
	}
	successor, err := service.Claim(ctx, target.ID, core.WorkOrderClaim{
		SessionID: "new-contract-session", ClientToken: "new-contract-token", ClaimantID: "worker-new", WorkerID: "worker-new",
		Agent: "codex", Model: "new-seat", Lease: time.Hour, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if successor.AttemptID == claimed.AttemptID || successor.RequiredModel != "new-seat" || successor.RequiredHarness != "claude" || successor.RequiredEffort != "high" {
		t.Fatalf("successor did not use revised contract: old=%+v new=%+v", claimed, successor)
	}
}
