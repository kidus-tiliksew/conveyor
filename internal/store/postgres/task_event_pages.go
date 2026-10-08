package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventWindowSelect seeks the (task_id, at, id) timeline index and charges
// stored source bytes in order. Only candidates inside both the row limit and
// the running byte budget return their variable-width columns; the lookahead
// and every candidate past the budget return fixed-width boundary and size
// metadata, so the driver never transfers them (component-mcp-investigation-reads).
const taskEventWindowSelect = `WITH candidates AS (
	SELECT e.id, e.at,
		octet_length(e.payload_json::text) + octet_length(e.kind) + octet_length(e.actor_id) + octet_length(e.actor_role)
			+ octet_length(COALESCE(e.job_id, '')) + octet_length(e.task_id) + 64 AS source_bytes
	FROM events e
	WHERE e.task_id = $1 AND e.id <= $2 AND ($3::text = '' OR e.kind = $3::text)%s
	ORDER BY e.at, e.id
	LIMIT $4
), budgeted AS (
	SELECT c.id, c.at, c.source_bytes,
		sum(c.source_bytes) OVER (ORDER BY c.at, c.id ROWS UNBOUNDED PRECEDING) <= $5 AS fits,
		row_number() OVER (ORDER BY c.at, c.id) < $4 AS within_limit
	FROM candidates c
)
SELECT b.id, b.at, b.source_bytes, b.fits,
	CASE WHEN b.fits AND b.within_limit THEN e.task_id ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN COALESCE(e.job_id, '') ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.kind ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.actor_id ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.actor_role ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.payload_json END
FROM budgeted b JOIN events e ON e.id = b.id
ORDER BY b.at, b.id`

// observeTaskEventWindowFetch, when set by a fixture, receives the
// variable-width bytes the driver delivered for each scanned candidate.
var observeTaskEventWindowFetch atomic.Pointer[func(eventID int64, variableBytes int)]

func (s *Store) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	if err := store.ValidateTaskEventWindowQuery(q); err != nil {
		return store.TaskEventWindow{}, err
	}
	// One short snapshot transaction makes the boundary count and the selected
	// rows a single consistent read view; nothing is held between calls.
	tx, err := s.beginWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
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
		if err := rows.Scan(&event.ID, &at, &size, &fits, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &role, &payload); err != nil {
			return store.TaskEventWindow{}, err
		}
		if observe := observeTaskEventWindowFetch.Load(); observe != nil {
			(*observe)(event.ID, len(event.TaskID)+len(event.JobID)+len(event.Kind)+len(event.ActorID)+len(role)+len(payload))
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
