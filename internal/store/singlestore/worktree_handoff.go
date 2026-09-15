package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"time"
)

func (s *Store) WorktreeHandoffCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaimIdentity, repository string, request core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	var result core.WorktreeHandoff
	err := s.orderTx(ctx, id, func(tx *sql.Tx, order core.WorkOrder) error {
		if !lease.ValidForCommand(order.TaskID, string(store.WorktreeHandoffCommand)) {
			return fmt.Errorf("worktree handoff requires taskops lease")
		}
		task, err := getTaskRow(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		var writes []core.Event
		result, writes, err = store.EvaluateWorktreeHandoff(task, order, claim, repository, events, request, time.Now().UTC())
		if err != nil {
			return err
		}
		for _, event := range writes {
			if err = taskEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}
