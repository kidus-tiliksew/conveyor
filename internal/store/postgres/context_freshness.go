package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) RecordContextObservation(ctx context.Context, lease taskops.TaskLease, observation core.ContextObservation) (core.ContextObservation, error) {
	var result core.ContextObservation
	id := observation.WorkOrderID
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
		if err != nil {
			return err
		}
		if !lease.ValidForCommand(taskID, string(store.ContextObservationCommand)) {
			return fmt.Errorf("context observation requires taskops lease")
		}
		if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
			return err
		}
		order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
		if err != nil {
			return err
		}
		var task core.Task
		task.ID = taskID
		task.Workspace = workspace(ctx)
		if err = tx.QueryRow(ctx, "SELECT repo_name,branch FROM tasks WHERE workspace_id=$1 AND id=$2", workspace(ctx), taskID).Scan(&task.Repo, &task.Branch); err != nil {
			return err
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		owner := ""
		if order.WorkerID != "" {
			if err = tx.QueryRow(ctx, "SELECT COALESCE(owner_user_id,'') FROM workers WHERE workspace_id=$1 AND id=$2 AND revoked_at IS NULL", workspace(ctx), order.WorkerID).Scan(&owner); err != nil {
				return store.ErrWorkOrderClaimUnauthorized
			}
		}
		var event *core.Event
		result, event, err = store.EvaluateContextObservation(ctx, task, order, owner, events, observation, time.Now().UTC())
		if err != nil {
			return err
		}
		if event != nil {
			if err = insertEvent(ctx, q, *event); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) RecordInProcessContextObservation(ctx context.Context, lease taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	var result core.ContextObservation
	if !lease.ValidForCommand(o.TaskID, string(store.ContextObservationCommand)) {
		return result, store.ErrWorkOrderClaimUnauthorized
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), o.TaskID); err != nil {
			return err
		}
		row, err := q.GetJob(ctx, db.GetJobParams{ID: o.JobID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(o.TaskID), WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		var event *core.Event
		result, event, err = store.EvaluateInProcessContextObservation(ctx, core.Task{ID: o.TaskID, Workspace: workspace(ctx)}, jobFromDB(row), events, o, time.Now().UTC())
		if err != nil {
			return err
		}
		if event != nil {
			if err = insertEvent(ctx, q, *event); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}
