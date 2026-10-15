package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventWindowSelect reads the boundary aggregate and the window candidates
// in one statement, which is SingleStore's consistent read view. Only
// candidates inside both the row limit and the running byte budget return
// their variable-width columns; the lookahead and every candidate past the
// budget return fixed-width boundary and size metadata, so the driver never
// transfers them (component-mcp-investigation-reads). The derived boundary
// row survives an empty window via LEFT JOIN.
//
// The selection direction (%[5]s) orders candidates, the running sum, the row
// limit, and the result alike, so a descending window charges newest first. A
// positive payload limit charges an over-limit row without its payload and
// never returns that payload column; the stored length travels as fixed-width
// metadata (task 261015-3721c6; component-mcp-investigation-reads
// "Descending, kind-set, and payload-limited windows").
const taskEventWindowSelect = `SELECT b.max_id, b.matching, w.id, w.at, w.source_bytes, w.fits, w.omitted, w.payload_bytes,
	CASE WHEN w.fits AND w.within_limit THEN e.task_id END,
	CASE WHEN w.fits AND w.within_limit THEN COALESCE(e.job_id, '') END,
	CASE WHEN w.fits AND w.within_limit THEN e.kind END,
	CASE WHEN w.fits AND w.within_limit THEN e.actor_id END,
	CASE WHEN w.fits AND w.within_limit THEN e.actor_role END,
	CASE WHEN w.fits AND w.within_limit AND NOT w.omitted THEN e.payload_json END
FROM (SELECT %[1]s AS max_id, COALESCE(SUM(CASE WHEN %[2]s THEN 1 ELSE 0 END), 0) AS matching
	FROM events WHERE workspace_id = ? AND task_id = ?%[3]s) b
LEFT JOIN (
	SELECT c.id, c.at, c.payload_bytes, c.omitted, c.source_bytes,
		SUM(c.source_bytes) OVER (ORDER BY c.at%[5]s, c.id%[5]s ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) <= ? AS fits,
		ROW_NUMBER() OVER (ORDER BY c.at%[5]s, c.id%[5]s) < ? AS within_limit
	FROM (SELECT r.id, r.at, r.payload_bytes, r.omitted,
			CASE WHEN r.omitted THEN 0 ELSE r.payload_bytes END + r.column_bytes AS source_bytes
		FROM (SELECT id, at, LENGTH(payload_json) AS payload_bytes,
				? > 0 AND LENGTH(payload_json) > ? AS omitted,
				LENGTH(kind) + LENGTH(actor_id) + LENGTH(actor_role) + LENGTH(COALESCE(job_id, '')) + LENGTH(task_id) + 64 AS column_bytes
			FROM events WHERE workspace_id = ? AND task_id = ? AND %[2]s%[4]s
			ORDER BY at%[5]s, id%[5]s LIMIT ?) r) c
) w ON w.id <= b.max_id
LEFT JOIN events e ON e.workspace_id = ? AND e.task_id = ? AND e.id = w.id
ORDER BY w.at%[5]s, w.id%[5]s`

// taskEventKindFilter renders the query's kind filter with its arguments: an
// exact kind (empty matches every kind) or a kind set.
func taskEventKindFilter(q store.TaskEventWindowQuery) (string, []any) {
	if len(q.Kinds) > 0 {
		args := make([]any, len(q.Kinds))
		for i, kind := range q.Kinds {
			args[i] = kind
		}
		return "kind IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(q.Kinds)), ", ") + ")", args
	}
	return "(? = '' OR kind = ?)", []any{q.Kind, q.Kind}
}

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
	kindFilter, kindArgs := taskEventKindFilter(q)
	maxID, boundaryCeiling, windowFilter := "COALESCE(MAX(id), 0)", "", ""
	args := []any{}
	if q.Boundary != nil {
		maxID, boundaryCeiling = "?", " AND id <= ?"
		args = append(args, q.Boundary.MaxID)
	}
	args = append(append(args, kindArgs...), ws, q.TaskID)
	if q.Boundary != nil {
		args = append(args, q.Boundary.MaxID)
	}
	args = append(args, q.MaxBytes, q.Limit+1, q.PayloadLimit, q.PayloadLimit, ws, q.TaskID)
	args = append(args, kindArgs...)
	if q.Boundary != nil {
		windowFilter = " AND id <= ?"
		args = append(args, q.Boundary.MaxID)
	}
	switch {
	case q.After != nil:
		windowFilter += " AND (at > ? OR at = ? AND id > ?)"
		args = append(args, q.After.At, q.After.At, q.After.ID)
	case q.Before != nil:
		windowFilter += " AND (at < ? OR at = ? AND id < ?)"
		args = append(args, q.Before.At, q.Before.At, q.Before.ID)
	}
	direction := ""
	if q.Descending {
		direction = " DESC"
	}
	args = append(args, q.Limit+1, ws, q.TaskID)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(taskEventWindowSelect, maxID, kindFilter, boundaryCeiling, windowFilter, direction), args...)
	if err != nil {
		return store.TaskEventWindow{}, translateBackendConflict(err)
	}
	defer rows.Close()
	window := store.TaskEventWindow{Events: []core.Event{}}
	captured := false
	for rows.Next() {
		var (
			boundary                         store.TaskEventBoundary
			id, size, payloadBytes           sql.NullInt64
			taskID, jobID, kind, actor, role sql.NullString
			at                               sql.NullTime
			fits, omitted                    sql.NullBool
			payload                          []byte
		)
		if err := rows.Scan(&boundary.MaxID, &boundary.Count, &id, &at, &size, &fits, &omitted, &payloadBytes, &taskID, &jobID, &kind, &actor, &role, &payload); err != nil {
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
		if omitted.Bool {
			if window.OmittedPayloadBytes == nil {
				window.OmittedPayloadBytes = map[int64]int{}
			}
			window.OmittedPayloadBytes[id.Int64] = int(payloadBytes.Int64)
			payload = nil
		}
		window.Events = append(window.Events, core.Event{ID: id.Int64, TaskID: taskID.String, JobID: jobID.String, Kind: kind.String, ActorID: actor.String, ActorRole: core.ActorRole(role.String), At: at.Time, Payload: json.RawMessage(payload)})
	}
	if err := rows.Err(); err != nil {
		return store.TaskEventWindow{}, err
	}
	if !captured {
		return store.TaskEventWindow{}, fmt.Errorf("task event window returned no boundary")
	}
	if q.Descending {
		store.ReverseTaskEvents(window.Events)
	}
	return window, nil
}
