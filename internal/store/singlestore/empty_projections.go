package singlestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) ListCallerAttentionTaskPage(ctx context.Context, query store.CallerAttentionQuery) (store.TaskPage, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[store.TaskPage](), err
}
func (s *Store) ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]store.CheckpointContextCandidate, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[[]store.CheckpointContextCandidate](), err
}
func (s *Store) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) requireEmpty(ctx context.Context, table string) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	// table is always a literal supplied by the methods in this file.
	var found int
	err = s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM `%s` WHERE workspace_id=? LIMIT 1", table), ws).Scan(&found)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	return store.ErrNotImplemented
}
