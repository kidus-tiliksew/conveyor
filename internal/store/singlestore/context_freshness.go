package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) RecordContextObservation(ctx context.Context, lease taskops.TaskLease, observation core.ContextObservation) (core.ContextObservation, error) {
	var result core.ContextObservation
	id := observation.WorkOrderID
	err := s.orderTx(ctx, id, func(tx *sql.Tx, order core.WorkOrder) error {
		if !lease.ValidForCommand(order.TaskID, string(store.ContextObservationCommand)) {
			return fmt.Errorf("context observation requires taskops lease")
		}
		task, err := getTaskRow(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		owner := ""
		if order.WorkerID != "" {
			if err = documentRow(ctx, tx, "SELECT COALESCE(owner_user_id,'') FROM workers WHERE workspace_id=? AND id=? AND revoked_at IS NULL", documentWorkspace(ctx), order.WorkerID).Scan(&owner); err != nil {
				return store.ErrWorkOrderClaimUnauthorized
			}
		}
		var event *core.Event
		result, event, err = store.EvaluateContextObservation(ctx, task, order, owner, events, observation, time.Now().UTC())
		if err != nil {
			return err
		}
		if event != nil {
			if err = taskEvent(ctx, tx, *event); err != nil {
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
	err := s.taskTx(ctx, o.TaskID, func(tx *sql.Tx) error {
		job, err := scanJob(documentRow(ctx, tx, "SELECT "+jobColumns+" FROM jobs WHERE workspace_id=? AND id=? FOR UPDATE", documentWorkspace(ctx), o.JobID))
		if err != nil {
			return err
		}
		task, err := getTaskRow(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		var event *core.Event
		result, event, err = store.EvaluateInProcessContextObservation(ctx, task, job, events, o, time.Now().UTC())
		if err != nil {
			return err
		}
		if event != nil {
			return taskEvent(ctx, tx, *event)
		}
		return nil
	})
	return result, err
}
