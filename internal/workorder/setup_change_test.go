package workorder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	if err = st.CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	completed, err := st.ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "completed-session", ClientToken: "completed-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	completed.State = core.WorkOrderCompleted
	if err = st.UpdateWorkOrder(ctx, completed); err != nil {
		t.Fatal(err)
	}
	interrupted := orders[1]
	interrupted.RetrySuppressed, interrupted.LastAttemptOutcome = true, core.WorkOrderOutcomeExpired
	if err = st.UpdateWorkOrder(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}, st, ctx, task, old, next
}

func TestChangeTaskSetupReconcilesInterruptedRoundAndIsIdempotent(t *testing.T) {
	service, st, ctx, task, _, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude", Effort: "high"}})
	ordersBefore, _ := st.ListTaskWorkOrders(ctx, task.ID)
	retained := ordersBefore[0]
	if err := st.AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: retained.JobID, ReviewWorkOrderID: retained.ID,
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
	claimed, err := st.ClaimWorkOrder(ctx, updated.ID, core.WorkOrderClaim{SessionID: "fresh-review", ClientToken: "fresh-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = st.UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err = st.AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: claimed.JobID, ReviewWorkOrderID: claimed.ID,
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
}

func TestChangeTaskSetupRejectsClaimedAttemptWithoutMutation(t *testing.T) {
	service, st, ctx, task, old, next := setupChangeFixture(t, []config.ReviewSeat{{Model: "stable", Harness: "codex"}, {Model: "new-seat", Harness: "claude"}})
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	for _, order := range orders {
		if order.State == core.WorkOrderQueued {
			order.RetrySuppressed, order.LastAttemptOutcome = false, ""
			if err := st.UpdateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "active", ClientToken: "secret", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
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
