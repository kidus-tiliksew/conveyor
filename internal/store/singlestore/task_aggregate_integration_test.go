package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func taskAggregateFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := integrationStore(t)
	ctx := store.WithWorkspace(t.Context(), "task-tests")
	if _, err := s.CreateWorkspace(ctx, "task-tests", "Tasks", &config.Config{Workspace: "task-tests"}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}
func aggregateTask(ctx context.Context, id string) core.Task {
	return core.Task{ID: id, Workspace: documentWorkspace(ctx), Branch: "conveyor/" + id, Title: id, State: core.TaskRunning, NextStage: core.StageImplement}
}
func taskTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskCallbackCleanupIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	taskTestOK(t, s.CreateTask(ctx, aggregateTask(ctx, "locked")))
	t.Run("cancellation holds until callback returns", func(t *testing.T) {
		ownerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		sentinel := errors.New("callback cancelled after cleanup")
		go func() {
			done <- s.WithTaskSideEffectLock(ownerCtx, "locked", func(context.Context) error { close(held); <-release; return sentinel })
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("callback did not acquire")
		}
		cancel()
		waitCtx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := s.SetTaskHold(waitCtx, "locked", true)
		stop()
		if err == nil {
			close(release)
			t.Fatal("cancellation released lock before callback returned")
		}
		close(release)
		if err = <-done; err != sentinel {
			t.Fatalf("callback error changed: %v", err)
		}
		_, err = s.SetTaskHold(ctx, "locked", true)
		taskTestOK(t, err)
	})
	t.Run("panic releases and escaped context cannot bypass", func(t *testing.T) {
		var escaped context.Context
		panicValue := &struct{}{}
		func() {
			defer func() {
				if recover() != panicValue {
					t.Error("callback panic changed")
				}
			}()
			_ = s.WithTaskSideEffectLock(ctx, "locked", func(inner context.Context) error { escaped = inner; panic(panicValue) })
		}()
		taskTestOK(t, s.WithTaskSideEffectLock(ctx, "locked", func(context.Context) error {
			timeout, cancel := context.WithTimeout(escaped, 200*time.Millisecond)
			defer cancel()
			_, err := s.SetTaskHold(timeout, "locked", false)
			if err == nil {
				return errors.New("escaped context bypassed new owner")
			}
			return nil
		}))
		_, err := s.SetTaskHold(ctx, "locked", false)
		taskTestOK(t, err)
	})
	t.Run("ownership does not transfer to another Store", func(t *testing.T) {
		other := &Store{db: s.db, log: s.log}
		taskTestOK(t, s.WithTaskSideEffectLock(ctx, "locked", func(inner context.Context) error {
			timeout, cancel := context.WithTimeout(inner, 200*time.Millisecond)
			defer cancel()
			_, err := other.SetTaskHold(timeout, "locked", true)
			if err == nil {
				return errors.New("another Store reused callback ownership")
			}
			return nil
		}))
	})
	if got := s.db.Stats().InUse; got != 0 {
		t.Fatalf("callback leaked %d connections", got)
	}
}

func TestTaskAggregateConstraintsIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	t.Run("invalid state and missing parent roll back", func(t *testing.T) {
		bad := aggregateTask(ctx, "bad-state")
		bad.State = "invented"
		if err := s.CreateTask(ctx, bad); err == nil {
			t.Fatal("invalid task state accepted")
		}
		bad = aggregateTask(ctx, "bad-parent")
		bad.ParentTaskID = "absent"
		if err := s.CreateTask(ctx, bad); err == nil {
			t.Fatal("missing parent accepted")
		}
		for _, id := range []string{"bad-state", "bad-parent"} {
			var count int
			taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id=?`, id).Scan(&count))
			if count != 0 {
				t.Fatal("invalid task left a projection")
			}
			events, err := s.ListEvents(ctx, id)
			taskTestOK(t, err)
			if len(events) != 0 {
				t.Fatal("invalid task left events")
			}
		}
	})
	t.Run("concurrent branch uniqueness", func(t *testing.T) {
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				task := aggregateTask(ctx, fmt.Sprintf("unique-%d", i))
				task.Branch = "same-branch"
				results <- s.CreateTask(ctx, task)
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		success := 0
		for err := range results {
			if err == nil {
				success++
			}
		}
		if success != 1 {
			t.Fatalf("uniqueness winners=%d", success)
		}
		var count int
		taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND kind='task.created'`, documentWorkspace(ctx)).Scan(&count))
		if count != 1 {
			t.Fatalf("events after competing create=%d", count)
		}
	})
	t.Run("work order parent and state validation", func(t *testing.T) {
		task := aggregateTask(ctx, "order-task")
		taskTestOK(t, s.CreateTask(ctx, task))
		job := core.Job{ID: "order-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		taskTestOK(t, s.CreateJob(ctx, job))
		for _, bad := range []core.WorkOrder{{ID: "bad-order-state", JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, State: "invented"}, {ID: "bad-order-parent", JobID: "absent", TaskID: task.ID, Stage: core.StageImplement}} {
			_, err := taskops.ExecuteWorkOrder(ctx, s, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) { return false, s.CreateWorkOrderCommand(ctx, lease, bad) })
			if err == nil {
				t.Fatal("invalid work order accepted")
			}
		}
	})
	t.Run("ledger rules reject mutation before SQL", func(t *testing.T) {
		for _, table := range []string{"events", "interventions"} {
			for _, op := range []string{"UPDATE", "DELETE"} {
				err := s.taskTx(ctx, "order-task", func(tx *sql.Tx) error {
					_, err := writeRow(ctx, tx, rowWrite{table: table, operation: op, values: map[string]any{"actor_id": "changed"}, where: map[string]any{"workspace_id": documentWorkspace(ctx)}})
					return err
				})
				if err == nil {
					t.Fatalf("%s %s accepted", op, table)
				}
			}
		}
	})
	t.Run("queue fold failure rolls back task and audit event", func(t *testing.T) {
		task := aggregateTask(ctx, "queue-failure")
		task.State = core.TaskQueued
		stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID)
		_, err := s.log.Append(ctx, documentWorkspace(ctx), stream, 0, []eventlog.NewEvent{{Kind: logqueue.KindEnqueued, Payload: []byte(`{"max_attempts":"invalid"}`), At: time.Now().UTC()}})
		taskTestOK(t, err)
		if err = s.CreateTask(ctx, task); err == nil {
			t.Fatal("malformed queue stream did not reject enqueue")
		}
		var count int
		taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), task.ID).Scan(&count))
		if count != 0 {
			t.Fatal("queue failure committed task")
		}
		events, err := s.ListEvents(ctx, task.ID)
		taskTestOK(t, err)
		if len(events) != 0 {
			t.Fatal("queue failure committed audit event")
		}
	})
}

func TestReviewAcceptanceTransactionIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	for _, verdict := range []string{"approve", "changes_requested"} {
		t.Run(verdict, func(t *testing.T) {
			task := aggregateTask(ctx, "review-"+verdict)
			task.NextStage = core.StageReview
			taskTestOK(t, s.CreateTask(ctx, task))
			job := core.Job{ID: task.ID + "-seat", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}
			taskTestOK(t, storetest.For(s).CreateReviewRound(ctx, task.ID, []core.Job{job}, []core.WorkOrder{order}))
			claimed, err := storetest.For(s).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: task.ID + "-session", ClientToken: task.ID + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
			taskTestOK(t, err)
			decision := core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: order.ID, ClaimSession: claimed.SessionID, ReviewRound: 1, ReviewSeat: 1, Verdict: verdict, ReasonCode: verdict, Summary: "checked", Feedback: "Update the implementation.", ReviewedCommitSHA: "head", PublicationEligible: true, MaxBounces: 3}
			// A rejected audit insert must leave the claimed attempt and all
			// acceptance projections intact for a retry with the same decision.
			bad := store.WithActor(ctx, store.Actor{ID: "bad\x00actor", Role: core.ActorUser})
			if err = taskops.New(s).AcceptReviewDecision(bad, decision); err == nil {
				t.Fatal("invalid audit actor accepted")
			}
			current, err := s.GetWorkOrder(ctx, order.ID)
			taskTestOK(t, err)
			if current.State != core.WorkOrderClaimed {
				t.Fatal("failed acceptance settled claim")
			}
			taskTestOK(t, taskops.New(s).AcceptReviewDecision(ctx, decision))
			taskTestOK(t, taskops.New(s).AcceptReviewDecision(ctx, decision))
			current, err = s.GetWorkOrder(ctx, order.ID)
			taskTestOK(t, err)
			if current.State != core.WorkOrderCompleted {
				t.Fatalf("accepted order=%s", current.State)
			}
			var publications int
			taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_publications WHERE workspace_id=? AND review_work_order_id=?`, documentWorkspace(ctx), order.ID).Scan(&publications))
			if publications != 1 {
				t.Fatalf("publication intents=%d", publications)
			}
			queued, err := logqueue.Load(ctx, s.log, documentWorkspace(ctx), logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), order.ID))
			taskTestOK(t, err)
			if !queued.Active() {
				t.Fatal("publication queue intent missing")
			}
			count, err := s.CountEvents(ctx, task.ID, "review.accepted")
			taskTestOK(t, err)
			if count != 1 {
				t.Fatalf("acceptance retry appended %d verdicts", count)
			}
			currentTask, err := s.GetTask(ctx, task.ID)
			taskTestOK(t, err)
			want := core.TaskAwaiting
			if verdict == "changes_requested" {
				want = core.TaskQueued
			}
			if currentTask.State != want {
				t.Fatalf("accepted task=%s want %s", currentTask.State, want)
			}
		})
	}
}
