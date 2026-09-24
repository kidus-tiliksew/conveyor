package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) RecordContextObservation(ctx context.Context, lease taskops.TaskLease, observation core.ContextObservation) (core.ContextObservation, error) {
	var result core.ContextObservation
	id := observation.WorkOrderID
	tx, err := s.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
	if err != nil {
		return result, err
	}
	if !lease.ValidForCommand(taskID, string(store.ContextObservationCommand)) {
		return result, fmt.Errorf("context observation requires taskops lease")
	}
	if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return result, err
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return result, err
	}
	var task core.Task
	task.ID = taskID
	task.Workspace = workspace(ctx)
	if err = tx.QueryRow(ctx, "SELECT repo_name,branch FROM tasks WHERE workspace_id=$1 AND id=$2", workspace(ctx), taskID).Scan(&task.Repo, &task.Branch); err != nil {
		return result, err
	}
	q := s.queries.WithTx(tx)
	rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return result, err
	}
	events := make([]core.Event, len(rows))
	for i := range rows {
		events[i] = eventFromDB(rows[i])
	}
	owner := ""
	if order.WorkerID != "" {
		if err = tx.QueryRow(ctx, "SELECT COALESCE(owner_user_id,'') FROM workers WHERE workspace_id=$1 AND id=$2 AND revoked_at IS NULL", workspace(ctx), order.WorkerID).Scan(&owner); err != nil {
			return result, store.ErrWorkOrderClaimUnauthorized
		}
	}
	result, event, err := store.EvaluateContextObservation(ctx, task, order, owner, events, observation, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if event != nil {
		if err = insertEvent(ctx, q, *event); err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}

func (s *Store) RecordInProcessContextObservation(ctx context.Context, lease taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	var result core.ContextObservation
	if !lease.ValidForCommand(o.TaskID, string(store.ContextObservationCommand)) {
		return result, store.ErrWorkOrderClaimUnauthorized
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), o.TaskID); err != nil {
		return result, err
	}
	q := s.queries.WithTx(tx)
	row, err := q.GetJob(ctx, db.GetJobParams{ID: o.JobID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return result, err
	}
	rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(o.TaskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return result, err
	}
	events := make([]core.Event, len(rows))
	for i := range rows {
		events[i] = eventFromDB(rows[i])
	}
	result, event, err := store.EvaluateInProcessContextObservation(ctx, core.Task{ID: o.TaskID, Workspace: workspace(ctx)}, jobFromDB(row), events, o, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if event != nil {
		if err = insertEvent(ctx, q, *event); err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}
