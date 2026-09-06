package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) UpdateTaskContext(ctx context.Context, taskID string, change store.TaskContextChange) (core.TaskContext, error) {
	add, err := store.NormalizeTaskContextInput(change.Add)
	if err != nil {
		return core.TaskContext{}, err
	}
	remove, err := store.NormalizeTaskContextInput(change.Remove)
	if err != nil {
		return core.TaskContext{}, err
	}
	err = s.taskTx(ctx, taskID, func(tx *sql.Tx) error {
		var state string
		if err := documentRow(ctx, tx, `SELECT state FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), taskID).Scan(&state); err != nil {
			return notFound(err, "task %s", taskID)
		}
		if core.TaskTerminal(core.TaskState(state)) {
			return store.ErrTaskTerminal
		}
		events, err := documentTaskEvents(ctx, tx, taskID)
		if err != nil {
			return err
		}
		activeRequirements, activeDesigns := store.ActiveTaskContextReferences(events)
		versions, err := validateTaskContextTx(ctx, tx, documentWorkspace(ctx), add)
		if err != nil {
			return err
		}
		for _, id := range remove.RequirementIDs {
			if !activeRequirements[id] {
				return &store.TaskContextReferenceError{Kind: "requirement", ID: id, Reason: "is not attached to this task"}
			}
		}
		for _, id := range remove.DesignIDs {
			if activeDesigns[id] == 0 {
				return &store.TaskContextReferenceError{Kind: "system design", ID: id, Reason: "is not attached to this task"}
			}
		}
		now := time.Now().UTC()
		appendEvent := func(kind string, payload map[string]any) error {
			return taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: kind, At: now, Payload: core.JSONPayload(payload)})
		}
		for _, id := range add.RequirementIDs {
			if err := appendEvent(store.TaskContextRequirementAdded, map[string]any{"id": id}); err != nil {
				return err
			}
		}
		for _, id := range remove.RequirementIDs {
			if err := appendEvent(store.TaskContextRequirementRemoved, map[string]any{"id": id}); err != nil {
				return err
			}
		}
		for _, id := range add.DesignIDs {
			if err := appendEvent(store.TaskContextDesignAdded, map[string]any{"id": id, "version": versions[id]}); err != nil {
				return err
			}
			activeDesigns[id] = versions[id]
		}
		for _, id := range remove.DesignIDs {
			version := activeDesigns[id]
			if version == 0 {
				version = versions[id]
			}
			if err := appendEvent(store.TaskContextDesignRemoved, map[string]any{"id": id, "version": version}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return core.TaskContext{}, fmt.Errorf("update task context: %w", err)
	}
	return store.TaskContextForTask(ctx, s, taskID)
}

func repinTaskDesignContextTx(ctx context.Context, tx *sql.Tx, ws, id string, at time.Time) error {
	events, err := documentTaskEvents(ctx, tx, id)
	if err != nil {
		return err
	}
	_, pinned := store.ActiveTaskContextReferences(events)
	confirmed := map[string]int{}
	for id := range pinned {
		var current sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT current_version FROM system_designs WHERE workspace_id=? AND id=?`, ws, id).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if current.Valid {
			confirmed[id] = int(current.Int64)
		}
	}
	for _, d := range store.AdvancedTaskContextDesignPins(pinned, confirmed) {
		if err := taskEvent(ctx, tx, core.Event{TaskID: id, Kind: store.TaskContextDesignAdded, At: at, Payload: core.JSONPayload(map[string]any{"id": d.ID, "version": d.Version})}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ProposeTaskContext(ctx context.Context, input core.TaskContextProposalInput) (core.TaskContextProposal, bool, error) {
	return s.proposeTaskContext(ctx, input, false)
}
func (s *Store) ConfirmTaskContextProposal(ctx context.Context, id string, kind core.TaskContextProposalTargetKind, target string) (core.TaskContextProposal, error) {
	return s.transitionTaskContextProposal(ctx, id, kind, target, core.TaskContextProposalConfirmed, false)
}
func (s *Store) DismissTaskContextProposal(ctx context.Context, id string, kind core.TaskContextProposalTargetKind, target string) (core.TaskContextProposal, error) {
	return s.transitionTaskContextProposal(ctx, id, kind, target, core.TaskContextProposalDismissed, false)
}
func (s *Store) ListTaskContextProposals(ctx context.Context, id string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	q := taskContextProposalSelect + ` WHERE workspace_id=?`
	args := []any{documentWorkspace(ctx)}
	if id != "" {
		q += ` AND task_id=?`
		args = append(args, id)
	}
	if state != "" {
		q += ` AND state=?`
		args = append(args, state)
	}
	q += ` ORDER BY target_id,task_id,target_kind`
	rows, err := documentRows(ctx, s.db, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.TaskContextProposal{}
	for rows.Next() {
		p, err := scanTaskContextProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
