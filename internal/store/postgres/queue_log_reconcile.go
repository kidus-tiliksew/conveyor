package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ReconcileQueuedTasks repairs projection/queue drift: a queued task whose
// job stream is no longer active is enqueued again, and a running task whose
// job was discarded after its final attempt is parked with evidence.
func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	repaired := 0
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:queue-reconcile:"+workspace(ctx)); err != nil {
			return err
		}
		taskIDs, err := s.queue.queuedTasksWithoutActiveDispatch(ctx, tx, workspace(ctx))
		if err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			inserted, err := s.enqueueTaskTx(ctx, tx, taskID, workspace(ctx))
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			repaired++
			if err := insertEvent(ctx, q, core.Event{
				TaskID: taskID, Kind: "dispatch.reconciled",
				Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"}),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return repaired, err
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
	}
	return repaired, nil
}

// enqueueTaskTx enqueues a dispatch inside the caller's transaction and
// reports whether a row was inserted. Duplicates are suppressed only while
// a dispatch is active or may retry, so an intentional human redispatch is
// never a silent no-op.
func (s *Store) enqueueTaskTx(ctx context.Context, tx pgx.Tx, taskID, workspace string) (bool, error) {
	return s.queue.enqueueDispatchTx(ctx, tx, workspace, taskID)
}
