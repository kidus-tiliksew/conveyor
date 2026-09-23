package singlestore

import (
	"context"
	"database/sql"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) completeVerificationTx(ctx context.Context, tx *sql.Tx, c store.VerificationCommand, order core.WorkOrder, rows []store.VerificationRow) error {
	task, err := getTaskRow(ctx, tx, order.TaskID)
	if err != nil {
		return err
	}
	job, err := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), order.JobID))
	if err != nil {
		return err
	}
	events, err := documentTaskEvents(ctx, tx, order.TaskID)
	if err != nil {
		return err
	}
	v, err := store.PrepareVerificationCompletion(ctx, task, order, job, rows, events, c, time.Now().UTC())
	if err != nil {
		return err
	}
	if err = orderWrite(ctx, tx, v.Order); err != nil {
		return err
	}
	if _, err = writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: jobValues(v.Job), where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": job.ID}}); err != nil {
		return err
	}
	if err = taskWrite(ctx, tx, task.ID, map[string]any{"state": v.Task.State, "next_stage": v.Task.NextStage, "recovery_stage": v.Task.RecoveryStage}); err != nil {
		return err
	}
	if v.Intervention != nil {
		if err = insertInterventionTx(ctx, tx, *v.Intervention); err != nil {
			return err
		}
	}
	for _, e := range v.Events {
		if err = taskEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	if v.Task.State == core.TaskQueued && !v.Order.RetrySuppressed {
		_, err = s.enqueueTaskTx(ctx, tx, task.ID, documentWorkspace(ctx))
	}
	return err
}
