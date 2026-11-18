package singlestore

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerTaskChunk bounds one per-task marker statement. Each chunk statement
// carries one top-one arm per task, so the arms stay below the statement
// limit and the read stays proportional to the requested tasks
// (activity-marker-bounds).
var markerTaskChunk = 100

func markerTaskBatches(taskIDs []string) [][]string {
	if len(taskIDs) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(taskIDs)+markerTaskChunk-1)/markerTaskChunk)
	for start := 0; start < len(taskIDs); start += markerTaskChunk {
		out = append(out, taskIDs[start:min(start+markerTaskChunk, len(taskIDs))])
	}
	return out
}

type markerRow struct {
	taskID      string
	state       core.TaskState
	latestStage string
	lastEventAt time.Time
	lastEventID int64
	createdAt   time.Time
}

func markerTaskRowsQuery(ws string, taskIDs []string) (string, []any) {
	if taskIDs == nil {
		return `SELECT id,state,created_at FROM tasks WHERE workspace_id=? ORDER BY created_at,id`, []any{ws}
	}
	query := `SELECT id,state,created_at FROM tasks WHERE workspace_id=? AND id IN (` + taskMapMarks(len(taskIDs)) + `) ORDER BY created_at,id`
	return query, append([]any{ws}, taskMapArgs(taskIDs)...)
}

// markerTaskRows is the one state/ID selection: every listed task's state
// and creation order, or the requested tasks' states. The last-event and
// last-stage summaries are separate bounded reads, so a task whose history
// is large still costs one row (activity-marker-bounds).
func (s *Store) markerTaskRows(ctx context.Context, taskIDs []string) ([]markerRow, error) {
	if taskIDs != nil {
		taskIDs = store.UniqueTaskIDs(taskIDs)
		if len(taskIDs) == 0 {
			return nil, nil
		}
	}
	query, args := markerTaskRowsQuery(documentWorkspace(ctx), taskIDs)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []markerRow{}
	for rows.Next() {
		var row markerRow
		if err = rows.Scan(&row.taskID, &row.state, &row.createdAt); err != nil {
			return nil, err
		}
		row.lastEventAt = row.createdAt
		out = append(out, row)
	}
	return out, rows.Err()
}

func markerLatestSummariesQuery(ws string, taskIDs []string) (string, []any) {
	arms := make([]string, 0, len(taskIDs))
	args := make([]any, 0, len(taskIDs)*8)
	for _, id := range taskIDs {
		arms = append(arms, `(SELECT t.task_id,e.at,e.id,j.stage AS job_stage,c.stage AS claim_stage FROM (SELECT ? AS task_id) t LEFT JOIN (SELECT at,id FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=? ORDER BY at DESC,id DESC LIMIT 1) e ON 1=1 LEFT JOIN (SELECT stage FROM jobs WHERE workspace_id=? AND task_id=? ORDER BY started_at DESC,id DESC LIMIT 1) j ON 1=1 LEFT JOIN (SELECT stage FROM work_orders WHERE workspace_id=? AND task_id=? AND state='claimed' ORDER BY execution_started_at DESC,created_at DESC,id DESC LIMIT 1) c ON 1=1)`)
		args = append(args, id, ws, id, id, ws, id, ws, id)
	}
	return `SELECT per_task.task_id,per_task.at,per_task.id,per_task.job_stage,per_task.claim_stage FROM (` + strings.Join(arms, " UNION ALL ") + `) per_task`, args
}

// markerLatestSummaries folds the latest claim stage, job stage, and event
// tuple into the rows. It reads at most one row per requested task and
// never ranks a task's full history.
func (s *Store) markerLatestSummaries(ctx context.Context, rows []markerRow) error {
	byTask := make(map[string]*markerRow, len(rows))
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].taskID
		byTask[rows[i].taskID] = &rows[i]
	}
	for _, chunk := range markerTaskBatches(ids) {
		if err := s.markerLatestTuples(ctx, byTask, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markerLatestTuples(ctx context.Context, byTask map[string]*markerRow, taskIDs []string) error {
	query, args := markerLatestSummariesQuery(documentWorkspace(ctx), taskIDs)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var at sql.NullTime
		var id sql.NullInt64
		var jobStage, claimStage sql.NullString
		if err = rows.Scan(&taskID, &at, &id, &jobStage, &claimStage); err != nil {
			return err
		}
		row := byTask[taskID]
		if row == nil {
			continue
		}
		if at.Valid {
			row.lastEventAt = at.Time
		}
		if id.Valid {
			row.lastEventID = id.Int64
		}
		if jobStage.Valid {
			row.latestStage = jobStage.String
		}
		if claimStage.Valid {
			row.latestStage = claimStage.String
		}
	}
	return rows.Err()
}

// markerSQL is the SingleStore spelling of the shared activity-marker fold
// read (store.MarkerFoldSQL). Snapshot bodies are not marker inputs and never
// cross the connection.
var markerSQL = store.MarkerSQLDialect{
	Lease:       store.SingleStoreLeaseSQL,
	Placeholder: func(int) string { return "?" },
	Scope: func(b *store.MarkerSQLBinder, ws string, page []string) string {
		marks := make([]string, len(page))
		workspace := b.Bind(ws)
		for i, id := range page {
			marks[i] = b.Bind(id)
		}
		return `FROM events_columnstore e
		WHERE e.workspace_id=` + workspace + ` AND e.task_id IS NOT NULL AND e.task_id IN (` + strings.Join(marks, ",") + `)`
	},
	Compact: compactEventPayloadColumn,
	InList: func(b *store.MarkerSQLBinder, col string, values []string) string {
		marks := make([]string, len(values))
		for i, value := range values {
			marks[i] = b.Bind(value)
		}
		return col + " IN (" + strings.Join(marks, ",") + ")"
	},
	Int:       func(placeholder string) string { return "CAST(" + placeholder + " AS SIGNED)" },
	Text:      func(json string) string { return json },
	Keys:      func(json string) string { return "JSON_KEYS(" + json + ")" },
	Field:     func(json, key string) string { return "JSON_EXTRACT_JSON(" + json + ",'" + key + "')" },
	FieldText: func(json, key string) string { return "JSON_EXTRACT_STRING(" + json + ",'" + key + "')" },
	IsString:  func(value string) string { return "(COALESCE(JSON_GET_TYPE(" + value + "),'')='string')" },
	Object:    func(pairs []string) string { return "JSON_BUILD_OBJECT(" + strings.Join(pairs, ",") + ")" },
	Contains:  func(text, literal string) string { return "(INSTR(" + text + "," + literal + ")>0)" },
	Lower:     func(text string) string { return "LOWER(" + text + ")" },
	// The binary cast keeps the replacement case-sensitive under any
	// collation.
	Stripped: func(text, literal string) string {
		return "LENGTH(REPLACE(CAST(" + text + " AS BINARY)," + literal + ",''))"
	},
	ASCII: func(text string) string { return "(LENGTH(" + text + ")=CHAR_LENGTH(" + text + "))" },
}

// markerFoldRows reads the fold inputs for the page's tasks in chunks, and
// reads back the claim histories whose newest claim does not supersede the
// claims the reduction left out (store.MarkerClaimBackfill).
func (s *Store) markerFoldRows(ctx context.Context, in store.MarkerFoldInput) ([]core.Event, error) {
	out := []core.Event{}
	for _, chunk := range markerTaskBatches(in.Page) {
		chunkSet := make(map[string]bool, len(chunk))
		for _, id := range chunk {
			chunkSet[id] = true
		}
		within := func(ids []string) []string {
			kept := make([]string, 0, len(ids))
			for _, id := range ids {
				if chunkSet[id] {
					kept = append(kept, id)
				}
			}
			return kept
		}
		part := store.MarkerFoldInput{Workspace: in.Workspace, Page: chunk, ReviewTasks: within(in.ReviewTasks), LiveReviewTasks: within(in.LiveReviewTasks), Now: in.Now}
		for _, order := range in.OpenReviewOrders {
			if chunkSet[order.TaskID] {
				part.OpenReviewOrders = append(part.OpenReviewOrders, order)
			}
		}
		query, args := store.MarkerFoldSQL(markerSQL, part)
		rows, err := s.markerFoldScan(ctx, query, args)
		if err != nil {
			return nil, err
		}
		if partitions := store.MarkerClaimBackfill(rows, part.OpenReviewOrders); len(partitions) > 0 {
			query, args = store.MarkerClaimHistorySQL(markerSQL, part, partitions)
			history, err := s.markerFoldScan(ctx, query, args)
			if err != nil {
				return nil, err
			}
			rows = store.MergeMarkerFoldRows(rows, history)
		}
		for i := range rows {
			out = append(out, rows[i].Event)
		}
	}
	// UNION branches and distributed execution may arrive in physical order;
	// the lifecycle folds require the ledger's (at,id) order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID < out[j].ID
		}
		return out[i].At.Before(out[j].At)
	})
	return out, nil
}

func (s *Store) markerFoldScan(ctx context.Context, query string, args []any) ([]store.MarkerFoldRow, error) {
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.MarkerFoldRow{}
	for rows.Next() {
		var row store.MarkerFoldRow
		if err = rows.Scan(&row.Event.ID, &row.Event.TaskID, &row.Event.JobID, &row.Event.Kind, &row.Event.Payload, &row.Event.At, &row.Dropped, &row.DroppedImplement, &row.FutureDropped); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
