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

// Snapshot bodies are not marker inputs and must not cross the connection.
const markerEventPayloadColumn = compactEventPayloadColumn

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

func markerKindLiterals(kinds []string) string {
	quoted := make([]string, len(kinds))
	for i, kind := range kinds {
		quoted[i] = "'" + strings.ReplaceAll(kind, "'", "''") + "'"
	}
	return strings.Join(quoted, ",")
}

// markerForgePageKinds are the forge failure kinds read for the page's
// tasks. work_order.updated is read separately, only for an implement
// order's submission, exactly as store.LatestForgeFailure reads it.
func markerForgePageKinds() []string {
	kinds := make([]string, 0, len(activityMarkerForgeKinds))
	for _, kind := range activityMarkerForgeKinds {
		if kind != "work_order.updated" {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// markerLifecycleKinds are the event kinds the review lease folds read: the
// claim that opens a review lease and the renewals that extend it.
const markerLifecycleKinds = `'work_order.claimed','work_order.lease_renewed'`

// markerFoldQuery retains the last valid renewal for diagnostics and the
// earliest future renewal for the validator's lease horizon. Only the simple
// renewal payload written by the store is eligible: other JSON shapes retain
// their complete history for the Go decoder (including duplicate/escaped keys,
// payload identities, malformed leases, and whole-payload decoding errors).
func markerFoldQuery(ws string, pageTaskIDs, reviewTaskIDs, liveReviewTaskIDs []string, now time.Time) (string, []any) {
	args := []any{ws}
	args = append(args, taskMapArgs(pageTaskIDs)...)
	reviewBranch := ""
	if len(reviewTaskIDs) > 0 {
		reviewBranch = ` OR (e.kind IN ('work_order.released','review.completed','review.accepted','task.setup.changed') AND e.task_id IN (` + taskMapMarks(len(reviewTaskIDs)) + `))`
		args = append(args, taskMapArgs(reviewTaskIDs)...)
	}
	liveBranch := ""
	if len(liveReviewTaskIDs) > 0 {
		liveBranch = ` OR (e.kind IN ('work_order.claimed','work_order.lease_renewed') AND e.task_id IN (` + taskMapMarks(len(liveReviewTaskIDs)) + `))`
		args = append(args, taskMapArgs(liveReviewTaskIDs)...)
	}
	query := `SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + markerEventPayloadColumn + ` AS event_payload,e.at AS event_at
		FROM events_columnstore e
		WHERE e.workspace_id=? AND e.task_id IS NOT NULL AND e.task_id IN (` + taskMapMarks(len(pageTaskIDs)) + `)
		AND (
			e.kind IN ('pipeline.bounced','work_order.claimed','work_order.updated') OR
			(e.kind IN (` + markerKindLiterals(markerForgePageKinds()) + `))` + reviewBranch + liveBranch + `
		)
		ORDER BY event_at,id`
	if len(liveReviewTaskIDs) == 0 {
		return query, args
	}
	// This restricted shape has no payload identity and no typed field other
	// than the lease. Matching by job and decoding the lease therefore have
	// the same meaning as the Go lifecycle folds. Do not broaden it to arbitrary
	// objects merely because JSON_EXTRACT_STRING can read their lease field.
	const attemptField = `"attempt_id"[[:space:]]*:[[:space:]]*"[^"]*"`
	const leaseField = `"lease_expires_at"[[:space:]]*:[[:space:]]*"[0-9T:Z+.,-]+"`
	const comma = `[[:space:]]*,[[:space:]]*`
	const renewalShape = `^[[:space:]]*[{][[:space:]]*(` + attemptField + comma + leaseField + `|` + leaseField + `(` + comma + attemptField + `){0,1})[[:space:]]*[}][[:space:]]*$`
	const carry = "id,task_id,job_id,event_payload,event_at"
	instants := store.SingleStoreLeaseSQL.LeaseInstantSQL(`SELECT `+carry+`,
		JSON_EXTRACT_STRING(event_payload,'lease_expires_at') AS lease_text,
		CASE WHEN event_payload REGEXP '`+renewalShape+`'
			AND JSON_GET_TYPE(JSON_EXTRACT_JSON(event_payload,'lease_expires_at'))='string'
			THEN 1 ELSE 0 END AS lease_string
		FROM marker_rows WHERE kind='work_order.lease_renewed'`, carry)
	const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
	query = `WITH marker_rows AS (` + query + `),
		marker_clock AS (SELECT CAST(? AS SIGNED) AS now_sec,CAST(? AS SIGNED) AS now_nanos),
		renewal_instants AS (` + instants + `),
		ranked_renewals AS (
			SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,now_sec,now_nanos,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY lease_ok DESC,event_at DESC,id DESC) AS latest_rank,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY
					CASE WHEN lease_ok=1 AND ` + future + ` THEN 0 ELSE 1 END,
					lease_sec,lease_nanos,event_at,id) AS horizon_rank
			FROM renewal_instants CROSS JOIN marker_clock
		)
		SELECT id,task_id,job_id,kind,event_payload,event_at FROM marker_rows WHERE kind<>'work_order.lease_renewed'
		UNION ALL
		SELECT id,task_id,job_id,'work_order.lease_renewed',event_payload,event_at FROM ranked_renewals
		WHERE lease_ok=0 OR latest_rank=1 OR (horizon_rank=1 AND ` + future + `)
		ORDER BY event_at,id`
	return query, append(args, now.Unix(), now.Nanosecond())
}

// markerFoldRows reads the fold inputs for the page's tasks in chunks.
func (s *Store) markerFoldRows(ctx context.Context, pageTaskIDs, reviewTaskIDs, liveReviewTaskIDs []string, now time.Time) ([]core.Event, error) {
	out := []core.Event{}
	for _, chunk := range markerTaskBatches(pageTaskIDs) {
		chunkSet := make(map[string]bool, len(chunk))
		for _, id := range chunk {
			chunkSet[id] = true
		}
		review := make([]string, 0, len(reviewTaskIDs))
		for _, id := range reviewTaskIDs {
			if chunkSet[id] {
				review = append(review, id)
			}
		}
		live := make([]string, 0, len(liveReviewTaskIDs))
		for _, id := range liveReviewTaskIDs {
			if chunkSet[id] {
				live = append(live, id)
			}
		}
		query, args := markerFoldQuery(documentWorkspace(ctx), chunk, review, live, now)
		rows, err := documentRows(ctx, s.db, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var event core.Event
			if err = rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.Payload, &event.At); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, event)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
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
