package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func changeTaskSetup(t *testing.T, st *Store, ctx context.Context, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	t.Helper()
	return taskops.ExecuteSetupChange(ctx, st, request.TaskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
		return st.ChangeTaskSetupCommand(ctx, lease, request)
	})
}

func TestTaskSetupChangePersistenceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "setup-change-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Database: config.Database{Backend: "postgres"}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	old := config.ExecutionSetup{Name: "old", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", Effort: "low", TimeoutText: "1h"}}}
	next := old
	next.Name, next.ExecutionSettings.Implementation.Model, next.ExecutionSettings.Implementation.Effort = "next", "new", "high"
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, SetupName: old.Name, SetupContract: old, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "old", RequiredEffort: "low", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	desired := order
	desired.RequiredModel, desired.RequiredEffort, desired.QueueEnteredAt, desired.QueueDeadline = "new", "high", now.Add(time.Minute), now.Add(2*time.Hour)
	request := store.SetupChangeRequest{TaskID: task.ID, RequestID: "setup-pg-1", Reason: "repair routing", Setup: next, WorkOrderUpdates: []core.WorkOrder{desired}, ReviewTransition: "none"}
	if _, err = st.ChangeTaskSetupCommand(ctx, taskops.TaskLease{}, request); err == nil {
		t.Fatal("zero taskops lease authorized setup change")
	}
	unchanged, getErr := st.GetTask(ctx, task.ID)
	if getErr != nil || unchanged.SetupName != old.Name {
		t.Fatalf("unleased setup change mutated task=%+v err=%v", unchanged, getErr)
	}
	result, err := changeTaskSetup(t, st, ctx, request)
	if err != nil || result.Task.SetupName != next.Name || len(result.UpdatedWorkOrders) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	updated, _ := st.GetWorkOrder(ctx, order.ID)
	if updated.RequiredModel != "new" || updated.RequiredEffort != "high" {
		t.Fatalf("updated=%+v", updated)
	}
	duplicate, err := changeTaskSetup(t, st, ctx, request)
	if err != nil || duplicate.Task.SetupName != next.Name {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	conflict := request
	conflict.Reason = "different"
	if _, err = changeTaskSetup(t, st, ctx, conflict); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("conflict=%v", err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	var changes int
	for _, event := range events {
		if event.Kind == "task.setup.changed" {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("changes=%d", changes)
	}
}

func TestTaskSetupChangePostgresScopesExclusionToExecutingAttempts(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "setup-excl-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Database: config.Database{Backend: "postgres"}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	old := config.ExecutionSetup{Name: "old", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", TimeoutText: "1h"}}}
	next := old
	next.Name, next.ExecutionSettings.Implementation.Model = "next", "new"
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, SetupName: old.Name, SetupContract: old, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "delivered", ClientToken: "delivered-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	request := store.SetupChangeRequest{TaskID: task.ID, RequestID: "setup-pg-excl-claimed", Reason: "reroute", Setup: next, ReviewTransition: "none"}
	if _, err = changeTaskSetup(t, st, ctx, request); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("claimed attempt should block: err=%v", err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	request.RequestID = "setup-pg-excl-submitted"
	result, err := changeTaskSetup(t, st, ctx, request)
	if err != nil || result.Task.SetupName != next.Name {
		t.Fatalf("submitted implement attempt should not block: result=%+v err=%v", result, err)
	}
	untouched, _ := st.GetWorkOrder(ctx, order.ID)
	if untouched.State != core.WorkOrderSubmitted {
		t.Fatalf("delivered attempt mutated=%+v", untouched)
	}
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	reviewOrder := core.WorkOrder{ID: reviewJob.ID, TaskID: task.ID, JobID: reviewJob.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "seat", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, []core.Job{reviewJob}, []core.WorkOrder{reviewOrder}); err != nil {
		t.Fatal(err)
	}
	seat, err := storetest.For(st).ClaimWorkOrder(ctx, reviewOrder.ID, core.WorkOrderClaim{SessionID: "verdict", ClientToken: "verdict-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	seat.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, seat); err != nil {
		t.Fatal(err)
	}
	request.RequestID = "setup-pg-excl-verdict"
	if _, err = changeTaskSetup(t, st, ctx, request); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("in-flight review verdict should block: err=%v", err)
	}
}

func TestTaskSetupChangePostgresReaggregatesRetainedAndReplacementSeats(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "setup-review-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Database: config.Database{Backend: "postgres"}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldSetup := config.ExecutionSetup{Name: "old", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "old-one", Harness: "codex"}, {Model: "old-two", Harness: "codex"}}}}
	newSetup := config.ExecutionSetup{Name: "new", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "new-one", Harness: "claude"}, {Model: "new-two", Harness: "claude"}}}}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, SetupName: oldSetup.Name, SetupContract: oldSetup, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs := []core.Job{{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}, {ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}}
	orders := []core.WorkOrder{{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "old-one", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
		{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "old-two", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	completed, err := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "old-seat", ClientToken: "old-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	completed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, completed); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: completed.JobID, ReviewWorkOrderID: completed.ID, Verdict: "approve", ReasonCode: "approved", Summary: "old evidence", ReviewedCommitSHA: "head", ReviewRound: 1, ReviewSeat: 1, RequiredModel: completed.RequiredModel, RequiredHarness: completed.RequiredHarness, MaxBounces: 4}); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := st.GetWorkOrder(ctx, orders[1].ID)
	interrupted.RetrySuppressed, interrupted.LastAttemptOutcome = true, core.WorkOrderOutcomeExpired
	if err = storetest.For(st).UpdateWorkOrder(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	desired := interrupted
	desired.RequiredModel, desired.RequiredHarness = "new-two", "claude"
	desired.QueueEnteredAt, desired.QueueDeadline = now.Add(time.Minute), now.Add(2*time.Hour)
	replacementJob := core.Job{ID: jobs[0].ID + "-replacement", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	replacement := core.WorkOrder{ID: replacementJob.ID, TaskID: task.ID, JobID: replacementJob.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "new-one", RequiredHarness: "claude", RequiredEffort: "high", QueueEnteredAt: now.Add(time.Minute), QueueDeadline: now.Add(2 * time.Hour)}
	result, err := changeTaskSetup(t, st, ctx, store.SetupChangeRequest{TaskID: task.ID, RequestID: "review-reconcile", Reason: "replace review contract", Setup: newSetup,
		WorkOrderUpdates: []core.WorkOrder{desired}, NewJobs: []core.Job{replacementJob}, NewWorkOrders: []core.WorkOrder{replacement}, SupersedeWorkOrderIDs: []string{completed.ID}, ReviewTransition: "same_round_reconciled", PriorReviewRound: 1, ResultingReviewRound: 1})
	if err != nil || len(result.CreatedWorkOrders) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, id := range []string{replacement.ID, interrupted.ID} {
		order, getErr := st.GetWorkOrder(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if id == replacement.ID && order.RequiredEffort != "high" {
			t.Fatalf("replacement effort=%q", order.RequiredEffort)
		}
		claimed, claimErr := storetest.For(st).ClaimWorkOrder(ctx, id, core.WorkOrderClaim{SessionID: "fresh-" + id, ClientToken: "token-" + id, Lease: time.Hour, ExecutionTimeout: time.Hour})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		claimed.State = core.WorkOrderCompleted
		if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if err = storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: claimed.JobID, ReviewWorkOrderID: claimed.ID, Verdict: "approve", ReasonCode: "approved", Summary: "fresh evidence", ReviewedCommitSHA: "head", ReviewRound: 1, ReviewSeat: order.ReviewSeat, RequiredModel: order.RequiredModel, RequiredHarness: order.RequiredHarness, MaxBounces: 4}); err != nil {
			t.Fatal(err)
		}
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "review.round_completed"); countErr != nil || count != 1 {
		t.Fatalf("round completed=%d err=%v", count, countErr)
	}
}
