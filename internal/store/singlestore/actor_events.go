package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The shared event writers check the context actor before any SQL. An actor
// already named on the event envelope does not bypass the check, and the
// caller's transaction rolls back on refusal (component-persistence, Actor
// context; req-accounts-and-membership REQ-3).

func insertEvent(ctx context.Context, tx *sql.Tx, e core.Event) error {
	_, err := insertEventWithID(ctx, tx, e)
	return err
}
func insertEventWithID(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return 0, fmt.Errorf("task event %q: %w", e.Kind, err)
	}
	if e.TaskID == "" {
		return 0, fmt.Errorf("task-bound event requires a task id")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), e.TaskID).Scan(&exists); err != nil {
		return 0, notFound(err, "task %s", e.TaskID)
	}
	if err := insertWorkspaceEvent(ctx, tx, e); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&id)
	return id, err
}

func insertWorkspaceEvent(ctx context.Context, tx *sql.Tx, event core.Event) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return fmt.Errorf("workspace event %q: %w", event.Kind, err)
	}
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = json.RawMessage(`{}`)
	}
	result, err := writeRow(s2log.WithTx(ctx, tx), tx, rowWrite{table: "events", operation: "INSERT", values: map[string]any{"workspace_id": ws, "task_id": nullString(event.TaskID), "job_id": nullString(event.JobID), "kind": event.Kind, "actor_id": event.ActorID, "actor_role": string(event.ActorRole), "payload_json": []byte(event.Payload), "at": event.At}})
	if err != nil {
		return err
	}
	event.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	projection := store.ProjectLineageEvent(ws, event)
	for _, l := range projection.Suppresses {
		if _, err = tx.ExecContext(ctx, `DELETE FROM links WHERE workspace_id=? AND src_type=? AND src_id=? AND dst_type=? AND dst_id=? AND kind=?`, ws, l.SrcType, l.SrcID, l.DstType, l.DstID, l.Kind); err != nil {
			return err
		}
	}
	for _, l := range projection.Links {
		if err = insertLineageLink(ctx, tx, l); err != nil {
			return err
		}
	}
	return nil
}
