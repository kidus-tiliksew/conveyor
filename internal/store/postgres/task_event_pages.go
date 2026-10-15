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
//
// The selection direction (%[3]s) orders candidates, the running sum, the row
// limit, and the result alike, so a descending window charges newest first. A
// positive payload limit ($6) charges an over-limit row without its payload
// and never returns that payload column; the stored length travels as
// fixed-width metadata (task 261015-3721c6; component-mcp-investigation-reads
// "Descending, kind-set, and payload-limited windows").
const taskEventWindowSelect = `WITH candidates AS (
	SELECT e.id, e.at, octet_length(e.payload_json::text) AS payload_bytes,
		octet_length(e.kind) + octet_length(e.actor_id) + octet_length(e.actor_role)
			+ octet_length(COALESCE(e.job_id, '')) + octet_length(e.task_id) + 64 AS column_bytes
	FROM events e
	WHERE e.task_id = $1 AND e.id <= $2 AND %[1]s%[2]s
	ORDER BY e.at%[3]s, e.id%[3]s
	LIMIT $4
), charged AS (
	SELECT c.id, c.at, c.payload_bytes, $6::bigint > 0 AND c.payload_bytes > $6::bigint AS omitted, c.column_bytes
	FROM candidates c
), budgeted AS (
	SELECT c.id, c.at, c.payload_bytes, c.omitted,
		CASE WHEN c.omitted THEN 0 ELSE c.payload_bytes END + c.column_bytes AS source_bytes,
		sum(CASE WHEN c.omitted THEN 0 ELSE c.payload_bytes END + c.column_bytes) OVER (ORDER BY c.at%[3]s, c.id%[3]s ROWS UNBOUNDED PRECEDING) <= $5 AS fits,
		row_number() OVER (ORDER BY c.at%[3]s, c.id%[3]s) < $4 AS within_limit
	FROM charged c
)
SELECT b.id, b.at, b.source_bytes, b.fits, b.omitted, b.payload_bytes,
	CASE WHEN b.fits AND b.within_limit THEN e.task_id ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN COALESCE(e.job_id, '') ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.kind ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.actor_id ELSE '' END,
	CASE WHEN b.fits AND b.within_limit THEN e.actor_role ELSE '' END,
	CASE WHEN b.fits AND b.within_limit AND NOT b.omitted THEN e.payload_json END
FROM budgeted b JOIN events e ON e.id = b.id
ORDER BY b.at%[3]s, b.id%[3]s`

// taskEventKindFilter renders the query's kind filter over column as
// placeholder $n with its argument: an exact kind (empty matches every kind)
// or a kind set.
func taskEventKindFilter(q store.TaskEventWindowQuery, column string, n int) (string, any) {
	if len(q.Kinds) > 0 {
		return fmt.Sprintf("%s = ANY($%d::text[])", column, n), q.Kinds
	}
	return fmt.Sprintf("($%[2]d::text = '' OR %[1]s = $%[2]d::text)", column, n), q.Kind
}

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
		filter, kinds := taskEventKindFilter(q, "kind", 2)
		err = tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0), count(*) FILTER (WHERE `+filter+`) FROM events WHERE task_id = $1`, q.TaskID, kinds).Scan(&boundary.MaxID, &boundary.Count)
		if err != nil {
			return store.TaskEventWindow{}, err
		}
	} else {
		// IDs are allocated before commit, so a lower ID can appear after
		// capture. The append-only count at the ceiling detects it.
		boundary = *q.Boundary
		var count int
		filter, kinds := taskEventKindFilter(q, "kind", 3)
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id = $1 AND id <= $2 AND `+filter, q.TaskID, boundary.MaxID, kinds).Scan(&count); err != nil {
			return store.TaskEventWindow{}, err
		}
		if count != boundary.Count {
			return store.TaskEventWindow{}, store.ErrTaskEventHistoryChanged
		}
	}
	filter, kinds := taskEventKindFilter(q, "e.kind", 3)
	args := []any{q.TaskID, boundary.MaxID, kinds, q.Limit + 1, q.MaxBytes, q.PayloadLimit}
	seek, direction := "", ""
	switch {
	case q.After != nil:
		seek = ` AND (e.at, e.id) > ($7::timestamptz, $8::bigint)`
		args = append(args, q.After.At, q.After.ID)
	case q.Before != nil:
		seek = ` AND (e.at, e.id) < ($7::timestamptz, $8::bigint)`
		args = append(args, q.Before.At, q.Before.ID)
	}
	if q.Descending {
		direction = " DESC"
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(taskEventWindowSelect, filter, seek, direction), args...)
	if err != nil {
		return store.TaskEventWindow{}, err
	}
	defer rows.Close()
	window := store.TaskEventWindow{Boundary: boundary, Events: []core.Event{}}
	for rows.Next() {
		var (
			event        core.Event
			role         string
			at           time.Time
			size         int
			fits         bool
			omitted      bool
			payloadBytes int
			payload      []byte
		)
		if err := rows.Scan(&event.ID, &at, &size, &fits, &omitted, &payloadBytes, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &role, &payload); err != nil {
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
		if omitted {
			if window.OmittedPayloadBytes == nil {
				window.OmittedPayloadBytes = map[int64]int{}
			}
			window.OmittedPayloadBytes[event.ID] = payloadBytes
			payload = nil
		}
		event.ActorRole, event.At, event.Payload = core.ActorRole(role), at, json.RawMessage(payload)
		window.Events = append(window.Events, event)
	}
	if err := rows.Err(); err != nil {
		return store.TaskEventWindow{}, err
	}
	if q.Descending {
		store.ReverseTaskEvents(window.Events)
	}
	return window, nil
}
