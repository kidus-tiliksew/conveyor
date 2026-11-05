package singlestore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TestSubmissionGovernanceLockOrderIntegration proves submission governance
// takes the task-operation lock before the document lock, and runs it against
// the answered-authority clock, which takes them in the same order
// (component-persistence, Lock order).
func TestSubmissionGovernanceLockOrderIntegration(t *testing.T) {
	const (
		ws      = "lock-order"
		taskID  = "lock-order-task"
		orderID = "lock-order-task-implement-1"
		// settle lets a started path reach the lock it waits for. Too short a
		// settle can only hide an inversion, never fail correct code.
		settle = 500 * time.Millisecond
	)
	st := integrationStore(t)
	base := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	ctx, cancel := context.WithTimeout(base, time.Minute)
	defer cancel()
	if _, err := st.CreateWorkspace(ctx, ws, "Lock order", &config.Config{Workspace: ws}); err != nil {
		t.Fatal(err)
	}

	// The citation stays unanswered, so each clock tick takes the document lock
	// and leaves the order released.
	design, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-lock-order", Title: "Lock order", Category: "Component design"}, core.SystemDesignVersion{Content: "# Lock order\n\n```conveyor:governs\n- repo: repo\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, initial.Version); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "lock-order", Title: taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const worker, session = "worker-lock-order", "session-lock-order"
	if _, err = storetest.ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: worker, SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited revision.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: design.ID, CitedVersion: 1, StatementOrSectionID: "Lock order"}}}
	if _, err = storetest.ReleaseWorkerClaim(ctx, st, orderID, worker, core.WorkOrderRelease{SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: checkpoint, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}

	attach := func(ctx context.Context) error {
		_, err := st.AttachSubmissionGovernance(ctx, taskID, "repo", []string{"internal/store/locks.go"}, store.SubmissionGovernanceAttribution{})
		return err
	}
	tick := func(ctx context.Context) error {
		_, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		return err
	}
	hold := func(t *testing.T, ctx context.Context, key string) *sql.Tx {
		t.Helper()
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		if err = lockKey(ctx, tx, key); err != nil {
			t.Fatalf("lock %s: %v", key, err)
		}
		return tx
	}
	start := func(ctx context.Context, path func(context.Context) error) chan error {
		done := make(chan error, 1)
		go func() { done <- path(ctx) }()
		return done
	}
	// A first sequential pass compiles each path's plans, so settle covers lock
	// waits and not query compilation.
	if err = attach(ctx); err != nil {
		t.Fatal(err)
	}
	if err = tick(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("corpus lock stays free while attach waits for the task lock", func(t *testing.T) {
		holder := hold(t, ctx, "task-operation:"+ws+":"+taskID)
		attached := start(ctx, attach)
		select {
		case err := <-attached:
			t.Fatalf("attach returned while the task-operation lock was held: %v", err)
		case <-time.After(settle):
		}
		probeCtx, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
		defer cancelProbe()
		probe := hold(t, probeCtx, "document-corpus:"+ws)
		if err := probe.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := holder.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := <-attached; err != nil {
			t.Fatalf("attach after the task-operation lock was released: %v", err)
		}
	})

	// A held corpus lock queues attach and then the clock, so both real paths
	// overlap on one task and one document set. The clock waiting on that lock
	// shows it reached its document read. SingleStore's grant order picks which
	// path runs first, so this case proves only that neither fails with a lock
	// error; the case above is the one that proves the order.
	t.Run("attach and the clock finish after queueing on the corpus lock", func(t *testing.T) {
		pathsCtx, cancelPaths := context.WithTimeout(ctx, 20*time.Second)
		defer cancelPaths()
		holder := hold(t, ctx, "document-corpus:"+ws)
		attached := start(pathsCtx, attach)
		time.Sleep(settle)
		ticked := start(pathsCtx, tick)
		select {
		case err := <-ticked:
			t.Fatalf("clock returned while the corpus lock was held: %v", err)
		case <-time.After(settle):
		}
		if err := holder.Rollback(); err != nil {
			t.Fatal(err)
		}
		attachErr, tickErr := <-attached, <-ticked
		if attachErr != nil || tickErr != nil {
			t.Fatalf("attach err=%v tick err=%v", attachErr, tickErr)
		}
	})

	// A submission reaches attach under the side-effect lock of its own task,
	// and attach reuses that lock instead of waiting for it.
	t.Run("attach under the side-effect lock", func(t *testing.T) {
		if err := st.WithTaskSideEffectLock(ctx, taskID, attach); err != nil {
			t.Fatal(err)
		}
	})

	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if order.LastAttemptOutcome != core.WorkOrderOutcomeReleased || order.Checkpoint == nil {
		t.Fatalf("unanswered order was recovered: %+v", order)
	}
}
