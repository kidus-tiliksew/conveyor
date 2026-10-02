package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventWindowSelect reads the boundary aggregate and the window candidates
// in one statement, which is SingleStore's consistent read view. Only
// candidates inside both the row limit and the running byte budget return
// their variable-width columns; the lookahead and every candidate past the
// budget return fixed-width boundary and size metadata, so the driver never
// transfers them (component-mcp-protocol v14 MCP-READ-9). The derived boundary
// row survives an empty window via LEFT JOIN.
const taskEventWindowSelect = `SELECT b.max_id, b.matching, w.id, w.at, w.source_bytes, w.fits,
	CASE WHEN w.fits AND w.within_limit THEN e.task_id END,
	CASE WHEN w.fits AND w.within_limit THEN COALESCE(e.job_id, '') END,
	CASE WHEN w.fits AND w.within_limit THEN e.kind END,
	CASE WHEN w.fits AND w.within_limit THEN e.actor_id END,
	CASE WHEN w.fits AND w.within_limit THEN e.actor_role END,
	CASE WHEN w.fits AND w.within_limit THEN e.payload_json END
FROM (SELECT %s AS max_id, COALESCE(SUM(CASE WHEN ? = '' OR kind = ? THEN 1 ELSE 0 END), 0) AS matching
	FROM events WHERE workspace_id = ? AND task_id = ?%s) b
LEFT JOIN (
	SELECT c.id, c.at, c.source_bytes,
		SUM(c.source_bytes) OVER (ORDER BY c.at, c.id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) <= ? AS fits,
		ROW_NUMBER() OVER (ORDER BY c.at, c.id) < ? AS within_limit
	FROM (SELECT id, at,
			LENGTH(payload_json) + LENGTH(kind) + LENGTH(actor_id) + LENGTH(actor_role) + LENGTH(COALESCE(job_id, '')) + LENGTH(task_id) + 64 AS source_bytes
		FROM events WHERE workspace_id = ? AND task_id = ? AND (? = '' OR kind = ?)%s
		ORDER BY at, id LIMIT ?) c
) w ON w.id <= b.max_id
LEFT JOIN events e ON e.workspace_id = ? AND e.task_id = ? AND e.id = w.id
ORDER BY w.at, w.id`

// observeTaskEventWindowFetch, when set by a fixture, receives the
// variable-width bytes the driver delivered for each scanned candidate.
var observeTaskEventWindowFetch atomic.Pointer[func(eventID int64, variableBytes int)]

func (s *Store) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	if err := store.ValidateTaskEventWindowQuery(q); err != nil {
		return store.TaskEventWindow{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.TaskEventWindow{}, err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE workspace_id=? AND id=?`, ws, q.TaskID).Scan(&exists); err != nil {
		return store.TaskEventWindow{}, notFound(err, "task %s", q.TaskID)
	}
	// A captured boundary fixes the ceiling; the recount runs in the same
	// statement as the selection so a delayed lower-ID commit is detected
	// before its changed window is returned.
	maxID, boundaryCeiling, windowFilter := "COALESCE(MAX(id), 0)", "", ""
	args := []any{q.Kind, q.Kind, ws, q.TaskID}
	if q.Boundary != nil {
		maxID, boundaryCeiling = "?", " AND id <= ?"
		args = []any{q.Boundary.MaxID, q.Kind, q.Kind, ws, q.TaskID, q.Boundary.MaxID}
		windowFilter = " AND id <= ?"
	}
	args = append(args, q.MaxBytes, q.Limit+1, ws, q.TaskID, q.Kind, q.Kind)
	if q.Boundary != nil {
		args = append(args, q.Boundary.MaxID)
	}
	if q.After != nil {
		windowFilter += " AND (at > ? OR at = ? AND id > ?)"
		args = append(args, q.After.At, q.After.At, q.After.ID)
	}
	args = append(args, q.Limit+1, ws, q.TaskID)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(taskEventWindowSelect, maxID, boundaryCeiling, windowFilter), args...)
	if err != nil {
		return store.TaskEventWindow{}, translateBackendConflict(err)
	}
	defer rows.Close()
	window := store.TaskEventWindow{Events: []core.Event{}}
	captured := false
	for rows.Next() {
		var (
			boundary                         store.TaskEventBoundary
			id, size                         sql.NullInt64
			taskID, jobID, kind, actor, role sql.NullString
			at                               sql.NullTime
			fits                             sql.NullBool
			payload                          []byte
		)
		if err := rows.Scan(&boundary.MaxID, &boundary.Count, &id, &at, &size, &fits, &taskID, &jobID, &kind, &actor, &role, &payload); err != nil {
			return store.TaskEventWindow{}, err
		}
		if observe := observeTaskEventWindowFetch.Load(); observe != nil && id.Valid {
			(*observe)(id.Int64, len(taskID.String)+len(jobID.String)+len(kind.String)+len(actor.String)+len(role.String)+len(payload))
		}
		if !captured {
			captured = true
			if q.Boundary != nil && boundary.Count != q.Boundary.Count {
				return store.TaskEventWindow{}, store.ErrTaskEventHistoryChanged
			}
			window.Boundary = boundary
		}
		if !id.Valid {
			continue
		}
		if len(window.Events) == q.Limit {
			window.More = true
			break
		}
		if !fits.Bool {
			if len(window.Events) == 0 {
				return store.TaskEventWindow{}, &store.TaskEventTooLargeError{EventID: id.Int64, Bytes: int(size.Int64), Limit: q.MaxBytes}
			}
			window.More = true
			break
		}
		window.Events = append(window.Events, core.Event{ID: id.Int64, TaskID: taskID.String, JobID: jobID.String, Kind: kind.String, ActorID: actor.String, ActorRole: core.ActorRole(role.String), At: at.Time, Payload: json.RawMessage(payload)})
	}
	if err := rows.Err(); err != nil {
		return store.TaskEventWindow{}, err
	}
	if !captured {
		return store.TaskEventWindow{}, fmt.Errorf("task event window returned no boundary")
	}
	return window, nil
}
