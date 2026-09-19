package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) CreateTaskWithAttachments(ctx context.Context, t core.Task, ids []string, attached store.TaskContextInput, uploads []store.ArtifactUpload) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	if t.Workspace != ws {
		return fmt.Errorf("task workspace mismatch")
	}
	artifacts, err := store.PrepareIntakeArtifacts(t, uploads)
	if err != nil {
		return err
	}
	attached, err = store.NormalizeTaskContextInput(attached)
	if err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.NextStage == "" {
		t.NextStage = core.InitialStage(t.Level)
	}
	t.State = core.TaskClaiming
	return s.taskTx(ctx, t.ID, func(tx *sql.Tx) error {
		if err := s.createTaskTx(ctx, tx, t, ids, attached, nil); err != nil {
			return err
		}
		for i, a := range artifacts {
			a, err = prepareArtifact(ctx, a, uploads[i].Content)
			if err != nil {
				return err
			}
			if err = insertArtifactTx(ctx, tx, a, uploads[i].Content); err != nil {
				return err
			}
			if err = tx.QueryRowContext(ctx, `SELECT name,content_type,size_bytes,created_at FROM artifacts WHERE workspace_id=? AND id=?`, a.Workspace, a.ID).Scan(&a.Name, &a.ContentType, &a.SizeBytes, &a.CreatedAt); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, store.IntakeArtifactEvent(t, a)); err != nil {
				return err
			}
		}
		state, err := core.TransitionTask(t.State, core.TaskIntakeFinalize)
		if err != nil {
			return err
		}
		if err = taskWrite(ctx, tx, t.ID, map[string]any{"state": state}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, store.IntakeFinalizedEvent(t)); err != nil {
			return err
		}
		_, err = s.enqueueTaskTx(ctx, tx, t.ID, t.Workspace)
		return err
	})
}
