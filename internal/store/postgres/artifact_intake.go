package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// ART-STORE-1 (component-persistence): all intake writes share the queue's tx.
func (s *Store) CreateTaskWithAttachments(ctx context.Context, t core.Task, ids []string, attached store.TaskContextInput, uploads []store.ArtifactUpload) error {
	if t.Workspace != workspace(ctx) {
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
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := s.createTaskTx(ctx, tx, q, t, ids, attached, nil); err != nil {
			return err
		}
		for i, a := range artifacts {
			stored, err := s.createArtifactTx(ctx, tx, a, uploads[i].Content)
			if err != nil {
				return err
			}
			if err = insertEvent(ctx, q, store.IntakeArtifactEvent(t, stored)); err != nil {
				return err
			}
		}
		state, err := core.TransitionTask(t.State, core.TaskIntakeFinalize)
		if err != nil {
			return err
		}
		if _, err = q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: t.ID, WorkspaceID: t.Workspace, State: string(state)}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, store.IntakeFinalizedEvent(t)); err != nil {
			return err
		}
		_, err = s.enqueueTaskTx(ctx, tx, t.ID, t.Workspace)
		return err
	})
}
