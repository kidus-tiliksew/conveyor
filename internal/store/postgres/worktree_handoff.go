package postgres

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"time"
)

func (s *Store) WorktreeHandoffCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaimIdentity, repository string, request core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	var result core.WorktreeHandoff
	tx, err := s.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
	if err != nil {
		return result, err
	}
	if !lease.ValidForCommand(taskID, string(store.WorktreeHandoffCommand)) {
		return result, fmt.Errorf("worktree handoff requires taskops lease")
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
	result, writes, err := store.EvaluateWorktreeHandoff(task, order, claim, repository, events, request, time.Now().UTC())
	if err != nil {
		return result, err
	}
	for _, event := range writes {
		if err = insertEvent(ctx, q, event); err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}
