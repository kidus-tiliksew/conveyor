package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func occupyingOpenTaskID(ctx context.Context, tx pgx.Tx, repo, branch, exceptID string) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", nil
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE workspace_id=$1 AND repo_name=$2 AND branch=$3 AND state NOT IN ('merged','closed') AND id<>$4 LIMIT 1`, workspace(ctx), repo, branch, exceptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) AttachTaskBranch(ctx context.Context, taskID, branch string) (core.Task, error) {
	branch = strings.TrimSpace(branch)
	if !gitx.LegalBranchName(branch) {
		return core.Task{}, fmt.Errorf("%w", store.ErrInvalidBranch)
	}
	var result core.Task
	err := s.WithTaskSideEffectLock(ctx, taskID, func(ctx context.Context) error {
		task, err := s.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		return s.WithTaskSideEffectLock(ctx, store.BranchCloseLockKey(task.Repo, branch), func(ctx context.Context) error {
			return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
				var currentBranch, currentState, currentRepo, currentBase string
				err := tx.QueryRow(ctx, `SELECT branch, state, repo_name, base_branch FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), taskID).Scan(&currentBranch, &currentState, &currentRepo, &currentBase)
				if err != nil {
					return notFound(err, "task %s", taskID)
				}
				task := core.Task{ID: taskID, State: core.TaskState(currentState), Repo: currentRepo, Branch: currentBranch, BaseBranch: currentBase}
				var claimed, prOpened bool
				if err := tx.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND state='claimed'),
				EXISTS (SELECT 1 FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='pull_request.opened')`, workspace(ctx), taskID).Scan(&claimed, &prOpened); err != nil {
					return err
				}
				occupant, err := occupyingOpenTaskID(ctx, tx, currentRepo, branch, taskID)
				if err != nil {
					return err
				}
				if err := store.EvaluateTaskBranchAttach(task, branch, claimed, prOpened, occupant); err != nil {
					return err
				}
				if currentBranch == branch {
					loaded, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
					if err != nil {
						return err
					}
					result = taskFromDB(loaded)
					return nil
				}
				if _, err := tx.Exec(ctx, `UPDATE tasks SET branch=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), taskID, branch); err != nil {
					if occupant, lookErr := occupyingOpenTaskID(ctx, tx, currentRepo, branch, taskID); lookErr == nil && occupant != "" {
						return store.TaskBranchInUseError(branch, occupant)
					}
					return err
				}
				loaded, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
				if err != nil {
					return err
				}
				result = taskFromDB(loaded)
				return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "task.branch_attached", Payload: core.JSONPayload(map[string]string{"previous": currentBranch, "branch": branch})})
			})
		})
	})
	return result, err
}
