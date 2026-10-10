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

func changeTaskPolicy(t *testing.T, st *Store, ctx context.Context, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	t.Helper()
	return taskops.ExecuteSetupChange(ctx, st, request.TaskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
		return st.ChangeTaskPolicyCommand(ctx, lease, request)
	})
}

func TestTaskPolicyChangePostgresScopesExclusionToExecutingAttempts(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "policy-excl-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Database: config.Database{Backend: "postgres"}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	legacy := config.ExecutionSetup{Name: "legacy", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", TimeoutText: "1h"}}}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, SetupName: legacy.Name, SetupContract: legacy, CreatedAt: now}
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
	request := store.SetupChangeRequest{TaskID: task.ID, RequestID: "policy-pg-excl-claimed", Reason: "longer verification", Policy: &store.TaskPolicyChange{StageTimeouts: map[string]string{"verify": "2h"}}}
	if _, err = changeTaskPolicy(t, st, ctx, request); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("claimed attempt should block: err=%v", err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	request.RequestID = "policy-pg-excl-submitted"
	result, err := changeTaskPolicy(t, st, ctx, request)
	if err != nil || result.ReviewTransition != "policy_only" || result.Task.SetupContract.ExecutionSettings.Verify.TimeoutText != "2h" {
		t.Fatalf("submitted implement attempt should not block: result=%+v err=%v", result, err)
	}
	stored, err := st.GetTask(ctx, task.ID)
	if err != nil || stored.SetupName != legacy.Name || stored.SetupContract.ExecutionSettings.Implementation.TimeoutText != "1h" || stored.SetupContract.ExecutionSettings.Verify.TimeoutText != "2h" {
		t.Fatalf("stored policy name=%q contract=%+v err=%v", stored.SetupName, stored.SetupContract, err)
	}
	untouched, _ := st.GetWorkOrder(ctx, order.ID)
	if untouched.State != core.WorkOrderSubmitted {
		t.Fatalf("delivered attempt mutated=%+v", untouched)
	}
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	reviewOrder := core.WorkOrder{ID: reviewJob.ID, TaskID: task.ID, JobID: reviewJob.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
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
	request.RequestID = "policy-pg-excl-verdict"
	if _, err = changeTaskPolicy(t, st, ctx, request); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("in-flight review verdict should block: err=%v", err)
	}
}

// A review seat superseded by a retired setup change stays historical-only:
// round aggregation counts only the current seats. The supersession is seeded
// as the retired command left it, an immutable task.setup.changed event plus
// the review_superseded flag (component-persistence).
func TestHistoricalSetupSupersessionPostgresReaggregatesReplacementSeats(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "setup-history-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Database: config.Database{Backend: "postgres"}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	panel := config.ExecutionSetup{Name: "panel", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}}}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, SetupName: panel.Name, SetupContract: panel, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs := []core.Job{{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}, {ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}}
	orders := []core.WorkOrder{{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
		{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	approve := func(id string) {
		t.Helper()
		claimed, claimErr := storetest.For(st).ClaimWorkOrder(ctx, id, core.WorkOrderClaim{SessionID: "session-" + id, ClientToken: "token-" + id, Lease: time.Hour, ExecutionTimeout: time.Hour})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		claimed.State = core.WorkOrderCompleted
		if err := storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: claimed.JobID, ReviewWorkOrderID: claimed.ID, Verdict: "approve", ReasonCode: "approved", Summary: "evidence", ReviewedCommitSHA: "head", ReviewRound: 1, ReviewSeat: claimed.ReviewSeat, MaxBounces: 4}); err != nil {
			t.Fatal(err)
		}
	}
	approve(orders[0].ID)
	if _, err = st.pool.Exec(ctx, `UPDATE work_orders SET review_superseded=true WHERE workspace_id=$1 AND id=$2`, workspace, orders[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: "operator-pg", ActorRole: core.ActorHuman, At: now,
		Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "task_id": task.ID, "request_id": "retired-setup-change", "lifecycle_boundary": "future_work",
			"review_transition": map[string]any{"kind": "same_round_reconciled", "prior_round": 1, "resulting_round": 1, "retained_work_order_ids": []string{}, "superseded_work_order_ids": []string{orders[0].ID}, "created_work_order_ids": []string{orders[0].ID + "-replacement"}}})}); err != nil {
		t.Fatal(err)
	}
	replacementJob := core.Job{ID: orders[0].ID + "-replacement", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	replacement := core.WorkOrder{ID: replacementJob.ID, TaskID: task.ID, JobID: replacementJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if err = st.CreateJob(ctx, replacementJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	approve(orders[1].ID)
	if count, countErr := st.CountEvents(ctx, task.ID, "review.round_completed"); countErr != nil || count != 0 {
		t.Fatalf("superseded seat counted toward the round: completed=%d err=%v", count, countErr)
	}
	approve(replacement.ID)
	if count, countErr := st.CountEvents(ctx, task.ID, "review.round_completed"); countErr != nil || count != 1 {
		t.Fatalf("round completed=%d err=%v", count, countErr)
	}
}
