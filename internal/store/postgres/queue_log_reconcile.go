package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ReconcileQueuedTasks repairs projection/queue drift: a queued task whose
// job stream is no longer active is enqueued again, and a running task whose
// job was discarded after its final attempt is parked with evidence. Each
// repair is reported to the context's drift recorder; the start-up check
// records it as drift.repaired (component-durable-queue, Reconciliation
// between tasks and jobs; req-quiet-idle-runtime AC-4.1).
func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReconcileQueuedTasks")
	repaired := 0
	// Repairs are reported after the transaction commits, so a rolled-back
	// repair reports nothing (component-persistence, Drift repair reports).
	var enqueued []string
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		enqueued = enqueued[:0]
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:queue-reconcile:"+workspace(ctx)); err != nil {
			return err
		}
		taskIDs, err := s.queue.queuedTasksWithoutActiveDispatch(ctx, tx, workspace(ctx), func(taskID string) (bool, error) {
			return reconcileRiskHeldTx(ctx, tx, q, workspace(ctx), taskID)
		})
		if err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			inserted, err := s.enqueueTaskTx(ctx, tx, taskID, workspace(ctx))
			if err != nil {
				return err
			}
			if inserted {
				enqueued = append(enqueued, taskID)
			}
		}
		return nil
	})
	if err != nil {
		return repaired, err
	}
	for _, taskID := range enqueued {
		repaired++
		store.ReportDriftRepair(ctx, store.DriftQueuedTaskWithoutJob.For(taskID))
	}
	discarded, err := s.queue.exhaustedDispatches(ctx, workspace(ctx))
	if err != nil {
		return repaired, err
	}
	for _, candidate := range discarded {
		_, applyErr := taskops.New(s).Perform(ctx, candidate.taskID, taskops.Command{
			Kind: core.TaskDispatchFailFinal, RecoveryStage: candidate.stage, ProjectStages: true,
			FailureMessage: "the queue discarded the dispatch job after its final attempt",
			Attempt:        candidate.attempt, MaxAttempts: candidate.maxAttempts,
		})
		if applyErr != nil {
			return repaired, applyErr
		}
		repaired++
		store.ReportDriftRepair(ctx, store.DriftRunningTaskJobDiscarded.For(candidate.taskID))
	}
	return repaired, nil
}

// reconcileRiskHeldTx reports whether a queued review task waits for its
// head's risk order, read in the reconcile transaction: the task is loaded,
// and store.QueuedReviewRiskHold reads its recorded submission comparisons
// and risk orders only when its frozen mode is enforce. Dispatch applies the
// same hold to the same head, so reconcile repairs no task that dispatch
// would leave waiting (feature-risk-based-review RR-12;
// req-risk-based-review AC-1.6, AC-5.10; component-risk-review invariant 13;
// component-durable-queue "Reconciliation between tasks and jobs").
func reconcileRiskHeldTx(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID, taskID string) (bool, error) {
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspaceID})
	if err != nil {
		return false, notFound(err, "task %s", taskID)
	}
	_, held, err := store.QueuedReviewRiskHold(taskFromDB(row),
		func() ([]core.Event, error) { return submissionComparisonEventsTx(ctx, tx, workspaceID, taskID) },
		func() ([]core.RiskOrder, error) { return taskRiskOrdersTx(ctx, tx, workspaceID, taskID) })
	return held, err
}

// submissionComparisonEventsTx reads a task's recorded submission
// comparisons, oldest first, in the order ListEvents returns them. Each event
// carries only its kind and payload.
func submissionComparisonEventsTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) ([]core.Event, error) {
	rows, err := tx.Query(ctx, `SELECT e.payload_json FROM events e JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND t.workspace_id = $2 AND e.kind = $3
ORDER BY e.at, e.id`, taskID, workspaceID, store.SubmissionComparisonEventKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []core.Event{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		events = append(events, core.Event{TaskID: taskID, Kind: store.SubmissionComparisonEventKind, Payload: payload})
	}
	return events, rows.Err()
}

// enqueueTaskTx enqueues a dispatch inside the caller's transaction and
// reports whether a row was inserted. Duplicates are suppressed only while
// a dispatch is active or may retry, so an intentional human redispatch is
// never a silent no-op.
func (s *Store) enqueueTaskTx(ctx context.Context, tx pgx.Tx, taskID, workspace string) (bool, error) {
	return s.queue.enqueueDispatchTx(ctx, tx, workspace, taskID)
}
