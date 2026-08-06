package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func activatePendingTaskContextTx(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID, documentID string, version int, design bool) error {
	addedKind, removedKind, activeKind := store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved, store.TaskContextRequirementActive
	if design {
		addedKind, removedKind, activeKind = store.TaskContextDesignAdded, store.TaskContextDesignRemoved, store.TaskContextDesignActive
	}
	rows, err := tx.Query(ctx, `SELECT task_id,kind,payload_json FROM events
		WHERE workspace_id=$1 AND task_id IS NOT NULL AND kind=ANY($2::text[])
		  AND payload_json->>'id'=$3 ORDER BY task_id,id`, workspaceID, []string{addedKind, removedKind}, documentID)
	if err != nil {
		return err
	}
	byTask := map[string][]core.Event{}
	for rows.Next() {
		var taskID, kind string
		var payload json.RawMessage
		if err = rows.Scan(&taskID, &kind, &payload); err != nil {
			rows.Close()
			return err
		}
		byTask[taskID] = append(byTask[taskID], core.Event{TaskID: taskID, Kind: kind, Payload: payload})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for taskID, events := range byTask {
		if !store.PendingTaskContextAttachment(events, documentID, version, design) {
			continue
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: activeKind,
			Payload: core.JSONPayload(map[string]any{"id": documentID, "version": version})}); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskContextTx(ctx context.Context, tx pgx.Tx, workspaceID string, input store.TaskContextInput) (map[string]int, error) {
	for _, id := range input.RequirementIDs {
		var current pgtype.Int4
		err := tx.QueryRow(ctx, `SELECT current_version FROM requirements WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &store.TaskContextReferenceError{Kind: "requirement", ID: id, Reason: "was not found in this workspace"}
		}
		if err != nil {
			return nil, err
		}
		if !current.Valid || current.Int32 <= 0 {
			return nil, &store.TaskContextReferenceError{Kind: "requirement", ID: id, Reason: "has no confirmed version"}
		}
	}
	versions := map[string]int{}
	for _, id := range input.DesignIDs {
		var current pgtype.Int4
		err := tx.QueryRow(ctx, `SELECT current_version FROM system_designs WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &store.TaskContextReferenceError{Kind: "system design", ID: id, Reason: "was not found in this workspace"}
		}
		if err != nil {
			return nil, err
		}
		if !current.Valid || current.Int32 <= 0 {
			return nil, &store.TaskContextReferenceError{Kind: "system design", ID: id, Reason: "has no confirmed version"}
		}
		versions[id] = int(current.Int32)
	}
	return versions, nil
}

func (s *Store) UpdateTaskContext(ctx context.Context, taskID string, change store.TaskContextChange) (core.TaskContext, error) {
	add, err := store.NormalizeTaskContextInput(change.Add)
	if err != nil {
		return core.TaskContext{}, err
	}
	remove, err := store.NormalizeTaskContextInput(change.Remove)
	if err != nil {
		return core.TaskContext{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), taskID).Scan(&state); err != nil {
			return notFound(err, "task %s", taskID)
		}
		if core.TaskTerminal(core.TaskState(state)) {
			return store.ErrTaskTerminal
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: pgtype.Text{String: taskID, Valid: true}, WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		activeRequirements, activeDesigns := store.ActiveTaskContextReferences(events)
		versions, err := validateTaskContextTx(ctx, tx, workspace(ctx), add)
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
			return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: kind, At: now, Payload: core.JSONPayload(payload)})
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
