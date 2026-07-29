package postgres

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestReviewRoundRetryPersistenceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "review-retry-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour}}}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Branch: "conveyor/task-pg-retry", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs1 := []core.Job{
		{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
		{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
	}
	orders1 := []core.WorkOrder{
		{ID: jobs1[0].ID, TaskID: task.ID, JobID: jobs1[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
		{ID: jobs1[1].ID, TaskID: task.ID, JobID: jobs1[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
	}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs1, orders1); err != nil {
		t.Fatal(err)
	}
	first, err := storetest.For(st).ClaimWorkOrder(ctx, orders1[0].ID, core.WorkOrderClaim{SessionID: "seat-1", ClientToken: "token-1", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, orders1[1].ID, core.WorkOrderClaim{SessionID: "seat-2", ClientToken: "token-2", Lease: time.Minute, ExecutionTimeout: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	if timedOut, getErr := st.GetWorkOrder(ctx, orders1[1].ID); getErr != nil || timedOut.State != core.WorkOrderTimedOut {
		t.Fatalf("timed out=%+v err=%v", timedOut, getErr)
	}
	if count, clockErr := taskops.New(st).TickOrderClock(ctx, time.Now().UTC()); clockErr != nil || count != 1 {
		t.Fatalf("order clock count=%d err=%v", count, clockErr)
	}
	jobs2 := []core.Job{
		{ID: task.ID + "-review-2-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
		{ID: task.ID + "-review-2-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
	}
	orders2 := []core.WorkOrder{
		{ID: jobs2[0].ID, TaskID: task.ID, JobID: jobs2[0].ID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 1, RequiredModel: "current-a", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"current"}}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
		{ID: jobs2[1].ID, TaskID: task.ID, JobID: jobs2[1].ID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 2, RequiredModel: "current-b", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"current"}}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
	}
	requests := []store.ReviewRoundRetryRequest{
		{TaskID: task.ID, RequestID: "pg-retry-1", Reason: "seat 2 timed out", PriorRound: 1, PRHead: "head-pg"},
		{TaskID: task.ID, RequestID: "pg-retry-2", Reason: "seat 2 timed out", PriorRound: 1, PRHead: "head-pg"},
	}
	type retryOutcome struct {
		request store.ReviewRoundRetryRequest
		result  store.ReviewRoundRetryResult
		err     error
	}
	start := make(chan struct{})
	ready := sync.WaitGroup{}
	ready.Add(2)
	results := make(chan retryOutcome, 2)
	for _, request := range requests {
		request := request
		go func() {
			ready.Done()
			<-start
			result, retryErr := storetest.For(st).RetryReviewRound(ctx, request, jobs2, orders2)
			results <- retryOutcome{request: request, result: result, err: retryErr}
		}()
	}
	ready.Wait()
	close(start)
	var winner retryOutcome
	succeeded, conflicted := 0, 0
	for range 2 {
		outcome := <-results
		if outcome.err == nil {
			succeeded++
			winner = outcome
		} else if errors.Is(outcome.err, store.ErrReviewRetryConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected concurrent retry error=%v", outcome.err)
		}
	}
	if succeeded != 1 || conflicted != 1 || winner.result.NewRound != 2 || len(winner.result.WorkOrders) != 2 {
		t.Fatalf("succeeded=%d conflicted=%d winner=%+v", succeeded, conflicted, winner)
	}
	request := winner.request
	duplicate, err := storetest.For(st).RetryReviewRound(ctx, request, jobs2, orders2)
	if err != nil || duplicate.NewRound != 2 || len(duplicate.WorkOrders) != 2 {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	changed := request
	changed.Reason = "different"
	if _, err = storetest.For(st).RetryReviewRound(ctx, changed, jobs2, orders2); !errors.Is(err, store.ErrReviewRetryConflict) {
		t.Fatalf("changed request error=%v", err)
	}
	restarted, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, err := restarted.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(persisted) != 4 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if prior, getErr := restarted.GetWorkOrder(ctx, orders1[1].ID); getErr != nil || prior.State != core.WorkOrderTimedOut || prior.ReviewRound != 1 {
		t.Fatalf("prior=%+v err=%v", prior, getErr)
	}
}
