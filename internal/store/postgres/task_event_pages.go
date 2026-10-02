package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventWindowSelect seeks the (task_id, at, id) timeline index, then charges
// stored source bytes in order. Payloads past the running budget stay in the
// database; their rows only mark the window boundary (component-mcp-protocol
// v14 MCP-READ-9).
const taskEventWindowSelect = `WITH candidates AS (
	SELECT e.id, e.task_id, COALESCE(e.job_id, '') AS job_id, e.kind, e.actor_id, e.actor_role, e.at, e.payload_json,
		octet_length(e.payload_json::text) + octet_length(e.kind) + octet_length(e.actor_id) + octet_length(e.actor_role)
			+ octet_length(COALESCE(e.job_id, '')) + octet_length(e.task_id) + 64 AS source_bytes
	FROM events e
	WHERE e.task_id = $1 AND e.id <= $2 AND ($3::text = '' OR e.kind = $3::text)%s
	ORDER BY e.at, e.id
	LIMIT $4
), budgeted AS (
	SELECT c.*, sum(c.source_bytes) OVER (ORDER BY c.at, c.id ROWS UNBOUNDED PRECEDING) AS running FROM candidates c
)
SELECT id, task_id, job_id, kind, actor_id, actor_role, at, source_bytes, running <= $5,
	CASE WHEN running <= $5 THEN payload_json END
FROM budgeted ORDER BY at, id`

func (s *Store) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	if err := store.ValidateTaskEventWindowQuery(q); err != nil {
		return store.TaskEventWindow{}, err
	}
	// One short snapshot transaction makes the boundary count and the selected
	// rows a single consistent read view; nothing is held between calls.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.TaskEventWindow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), q.TaskID).Scan(&exists); err != nil {
		return store.TaskEventWindow{}, notFound(err, "task %s", q.TaskID)
	}
	var boundary store.TaskEventBoundary
	if q.Boundary == nil {
		err = tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0), count(*) FILTER (WHERE $2::text = '' OR kind = $2::text) FROM events WHERE task_id = $1`, q.TaskID, q.Kind).Scan(&boundary.MaxID, &boundary.Count)
		if err != nil {
			return store.TaskEventWindow{}, err
		}
	} else {
		// IDs are allocated before commit, so a lower ID can appear after
		// capture. The append-only count at the ceiling detects it.
		boundary = *q.Boundary
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id = $1 AND id <= $2 AND ($3::text = '' OR kind = $3::text)`, q.TaskID, boundary.MaxID, q.Kind).Scan(&count); err != nil {
			return store.TaskEventWindow{}, err
		}
		if count != boundary.Count {
			return store.TaskEventWindow{}, store.ErrTaskEventHistoryChanged
		}
	}
	args := []any{q.TaskID, boundary.MaxID, q.Kind, q.Limit + 1, q.MaxBytes}
	seek := ""
	if q.After != nil {
		seek = ` AND (e.at, e.id) > ($6::timestamptz, $7::bigint)`
		args = append(args, q.After.At, q.After.ID)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(taskEventWindowSelect, seek), args...)
	if err != nil {
		return store.TaskEventWindow{}, err
	}
	defer rows.Close()
	window := store.TaskEventWindow{Boundary: boundary, Events: []core.Event{}}
	for rows.Next() {
		var (
			event   core.Event
			role    string
			at      time.Time
			size    int
			fits    bool
			payload []byte
		)
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &role, &at, &size, &fits, &payload); err != nil {
			return store.TaskEventWindow{}, err
		}
		if len(window.Events) == q.Limit {
			window.More = true
			break
		}
		if !fits {
			if len(window.Events) == 0 {
				return store.TaskEventWindow{}, &store.TaskEventTooLargeError{EventID: event.ID, Bytes: size, Limit: q.MaxBytes}
			}
			window.More = true
			break
		}
		event.ActorRole, event.At, event.Payload = core.ActorRole(role), at, json.RawMessage(payload)
		window.Events = append(window.Events, event)
	}
	if err := rows.Err(); err != nil {
		return store.TaskEventWindow{}, err
	}
	return window, nil
}
