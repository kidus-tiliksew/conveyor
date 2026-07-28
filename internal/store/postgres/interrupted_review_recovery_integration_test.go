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

func TestInterruptedReviewRecoveryPersistenceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "interrupted-review-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour}}}, Repos: []config.Repo{{Name: "repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs := []core.Job{{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}, {ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}}
	orders := []core.WorkOrder{{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}, {ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	completed, err := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "completed", ClientToken: "completed-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	completed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, completed); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "expired", ClientToken: "expired-token", Lease: time.Nanosecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if expired, getErr := st.GetWorkOrder(ctx, orders[1].ID); getErr != nil || expired.State != core.WorkOrderQueued || !expired.RetrySuppressed {
		t.Fatalf("expired=%+v err=%v", expired, getErr)
	}
	if count, clockErr := taskops.New(st).TickOrderClock(ctx, time.Now().UTC()); clockErr != nil || count != 1 {
		t.Fatalf("order clock count=%d err=%v", count, clockErr)
	}
	requests := []store.InterruptedReviewRecoveryRequest{{TaskID: task.ID, RequestID: "recover-a", Round: 1}, {TaskID: task.ID, RequestID: "recover-b", Round: 1}}
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	results := make(chan error, 2)
	for _, request := range requests {
		request := request
		go func() {
			ready.Done()
			<-start
			_, recoverErr := storetest.For(st).RecoverInterruptedReviewRound(ctx, request, time.Hour)
			results <- recoverErr
		}()
	}
	ready.Wait()
	close(start)
	var succeeded, conflicted int
	for range 2 {
		resultErr := <-results
		if resultErr == nil {
			succeeded++
		} else if errors.Is(resultErr, store.ErrReviewRetryConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected recovery error: %v", resultErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	var winningRequest string
	if _, err = storetest.For(st).RecoverInterruptedReviewRound(ctx, requests[0], time.Hour); err == nil {
		winningRequest = requests[0].RequestID
	} else if _, err = storetest.For(st).RecoverInterruptedReviewRound(ctx, requests[1], time.Hour); err == nil {
		winningRequest = requests[1].RequestID
	}
	if winningRequest == "" {
		t.Fatal("winning idempotent request was not persisted")
	}
	retained, _ := st.GetWorkOrder(ctx, orders[0].ID)
	recovered, _ := st.GetWorkOrder(ctx, orders[1].ID)
	if retained.State != core.WorkOrderCompleted || recovered.State != core.WorkOrderQueued || recovered.RetrySuppressed || !recovered.Claimable {
		t.Fatalf("retained=%+v recovered=%+v", retained, recovered)
	}
}
