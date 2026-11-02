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
		key, err := awaitContextObservations(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		var event *core.Event
		result, event, err = store.EvaluateContextObservation(ctx, task, order, events, observation, time.Now().UTC())
		if err != nil {
			return err
		}
		if event != nil {
			return appendContextObservation(ctx, tx, key, *event)
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
		key, err := awaitContextObservations(ctx, tx, o.TaskID)
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
			return appendContextObservation(ctx, tx, key, *event)
		}
		return nil
	})
	return result, err
}

// contextObservationClaimKey names the claim row that names a task's latest
// context observation event (component-persistence, SingleStore claimed
// records).
func contextObservationClaimKey(workspace, taskID string) string {
	return "context-observation:" + workspace + ":" + taskID
}

// awaitContextObservations waits until the task's latest recorded
// observation is visible, so an identical replay finds it in the history.
func awaitContextObservations(ctx context.Context, tx *sql.Tx, taskID string) (string, error) {
	ws := documentWorkspace(ctx)
	key := contextObservationClaimKey(ws, taskID)
	_, _, err := awaitClaimedRecord(ctx, tx, key, ws, taskID)
	return key, err
}

// appendContextObservation appends event and names it on the claim row.
func appendContextObservation(ctx context.Context, tx *sql.Tx, key string, event core.Event) error {
	if err := taskEvent(ctx, tx, event); err != nil {
		return err
	}
	appended, err := lastInsertedRecord(ctx, tx, claimedEventsTable, documentWorkspace(ctx), true)
	if err != nil {
		return err
	}
	return recordClaimedRecord(ctx, tx, key, appended)
}
