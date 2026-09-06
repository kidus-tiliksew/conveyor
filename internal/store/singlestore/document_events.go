package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Document commands serialize their projections and event-derived lineage in
// one transaction (DEC-38; component-persistence; component-lineage).
func (s *Store) documentTx(ctx context.Context, fn func(*sql.Tx) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id=?`, ws).Scan(&exists); err != nil {
			return notFound(err, "workspace %s", ws)
		}

		if err := lockKey(ctx, tx, "document-corpus:"+ws); err != nil {
			return err
		}
		return fn(tx)
	})
}
func documentWorkspace(ctx context.Context) string { ws, _ := workspace(ctx); return ws }

type documentScanner interface{ Scan(...any) error }
type documentResult struct {
	documentScanner
	err error
}

func (r documentResult) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if p, ok := d.(*[]string); ok {
			dest[i] = jsonStrings{p}
		}
	}
	return translateBackendConflict(r.documentScanner.Scan(dest...))
}

type jsonStrings struct{ value *[]string }

func (j jsonStrings) Scan(src any) error {
	if src == nil {
		*j.value = nil
		return nil
	}
	var b []byte
	switch x := src.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		return fmt.Errorf("unexpected JSON array type %T", src)
	}
	return json.Unmarshal(b, j.value)
}

type documentResultSet struct{ *sql.Rows }

func (r *documentResultSet) Scan(dest ...any) error {
	return (documentResult{documentScanner: r.Rows}).Scan(dest...)
}
func documentRow(ctx context.Context, db s2log.Executor, query string, args ...any) documentScanner {
	if _, err := workspace(ctx); err != nil {
		return documentResult{err: err}
	}
	return documentResult{documentScanner: db.QueryRowContext(ctx, query, documentArgs(args)...)}
}
func documentRows(ctx context.Context, db s2log.Executor, query string, args ...any) (*documentResultSet, error) {
	if _, err := workspace(ctx); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, documentArgs(args)...)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	return &documentResultSet{rows}, nil
}
func documentExec(ctx context.Context, db s2log.Executor, query string, args ...any) (sql.Result, error) {
	if _, err := workspace(ctx); err != nil {
		return nil, err
	}
	result, err := db.ExecContext(ctx, query, documentArgs(args)...)
	return result, translateBackendConflict(err)
}
func documentArgs(args []any) []any {
	out := append([]any(nil), args...)
	for i, a := range out {
		if v, ok := a.([]string); ok {
			if v == nil {
				v = []string{}
			}
			b, _ := json.Marshal(v)
			out[i] = b
		}
	}
	return out
}
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, fmt.Sprintf(format, args...))
	}
	return err
}
func insertWorkspaceEvent(ctx context.Context, tx *sql.Tx, event core.Event) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	actor := store.ActorFromContext(ctx)
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
func insertLineageLink(ctx context.Context, tx *sql.Tx, l core.LineageLink) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE created_by_event_id=LEAST(COALESCE(created_by_event_id,VALUES(created_by_event_id)),VALUES(created_by_event_id)),created_at=LEAST(created_at,VALUES(created_at)),legacy_created_by_event=NULL`, l.Workspace, l.SrcType, l.SrcID, l.DstType, l.DstID, l.Kind, l.CreatedByEventID, l.CreatedAt)
	return err
}

func insertEvent(ctx context.Context, tx *sql.Tx, e core.Event) error {
	_, err := insertEventWithID(ctx, tx, e)
	return err
}
func insertEventWithID(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
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
func documentTaskEvents(ctx context.Context, db s2log.Executor, taskID string) ([]core.Event, error) {
	rows, err := documentRows(ctx, db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND task_id=? ORDER BY id`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var e core.Event
		if err = rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	return s.listActivityMarkers(ctx, nil)
}

func (s *Store) ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	if taskIDs != nil && len(taskIDs) == 0 {
		return []store.ActivityMarker{}, nil
	}
	return s.listActivityMarkers(ctx, taskIDs)
}

func (s *Store) listActivityMarkers(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	type markerRow struct {
		taskID      string
		latestStage string
		lastEventAt time.Time
		lastEventID int64
	}
	var rows []markerRow
	var selected *documentResultSet
	var err error
	query := `SELECT t.id,COALESCE((SELECT w.stage FROM work_orders w WHERE w.workspace_id=t.workspace_id AND w.task_id=t.id AND w.state='claimed' ORDER BY w.execution_started_at DESC,w.created_at DESC,w.id DESC LIMIT 1),(SELECT j.stage FROM jobs j WHERE j.workspace_id=t.workspace_id AND j.task_id=t.id ORDER BY j.started_at DESC,j.id DESC LIMIT 1),''),COALESCE((SELECT e.at FROM events e WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id ORDER BY e.id DESC LIMIT 1),t.created_at),COALESCE((SELECT e.id FROM events e WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id ORDER BY e.id DESC LIMIT 1),0) FROM tasks t WHERE t.workspace_id=?`
	if len(taskIDs) == 0 {
		selected, err = documentRows(ctx, s.db, query+` ORDER BY t.created_at,t.id`, documentWorkspace(ctx))
	} else {
		selected, err = documentBatchRows(ctx, s.db, query+` AND t.id IN (%s) ORDER BY t.created_at,t.id`, documentWorkspace(ctx), taskIDs)
	}

	if err != nil {
		return nil, err
	}
	for selected.Next() {
		var row markerRow
		if err = selected.Scan(&row.taskID, &row.latestStage, &row.lastEventAt, &row.lastEventID); err != nil {
			selected.Close()
			return nil, err
		}
		rows = append(rows, row)
	}
	if err = selected.Err(); err != nil {
		selected.Close()
		return nil, err
	}
	selected.Close()
	// Scope the order read to the requested tasks. The activity feed still
	// wants the whole workspace, but a Tasks page must not pull every
	// workspace order in only to discard most of it.
	if len(rows) == 0 {
		return []store.ActivityMarker{}, nil
	}
	var orders []core.WorkOrder
	if len(taskIDs) == 0 {
		orders, err = s.ListWorkOrders(ctx)
	} else {
		orders, err = s.ListWorkOrdersForTasks(ctx, taskIDs)
	}
	if err != nil {
		return nil, err
	}
	var implementTaskIDs []string
	seenImplementTask := map[string]bool{}
	for _, order := range orders {
		if order.Stage == core.StageImplement && !seenImplementTask[order.TaskID] {
			implementTaskIDs = append(implementTaskIDs, order.TaskID)
			seenImplementTask[order.TaskID] = true
		}
	}
	blockersByTask := map[string]store.DependencyBlockers{}
	if len(implementTaskIDs) > 0 {
		blockersByTask, err = s.ListDependencyBlockers(ctx, implementTaskIDs)
		if err != nil {
			return nil, err
		}
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	hasReviewOrders := false
	reviewTaskIDs := make([]string, 0)
	seenReviewTask := map[string]bool{}
	for _, order := range orders {
		if order.Stage == core.StageImplement {
			blockers := blockersByTask[order.TaskID]
			order.BlockingTaskIDs = append([]string(nil), blockers.BlockingTaskIDs...)
			order.UnsatisfiableTaskIDs = append([]string(nil), blockers.UnsatisfiableTaskIDs...)
			if len(order.BlockingTaskIDs) > 0 {
				order.Claimable = false
			}
		}
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
		hasReviewOrders = hasReviewOrders || order.Stage == core.StageReview
		if order.Stage == core.StageReview && (order.State == core.WorkOrderClaimed || order.State == core.WorkOrderQueued) &&
			!order.ExecutionStartedAt.IsZero() && !seenReviewTask[order.TaskID] {
			reviewTaskIDs = append(reviewTaskIDs, order.TaskID)
			seenReviewTask[order.TaskID] = true
		}
	}
	eventsByTask := make(map[string][]core.Event)
	requestEventsByTask := make(map[string][]core.Event)
	forgeEventsByTask := make(map[string][]core.Event)
	markerQuery := `SELECT e.id,e.task_id,COALESCE(e.job_id,''),e.kind,e.payload_json,e.at FROM events e WHERE e.workspace_id=? AND e.task_id IS NOT NULL`
	var markerRows *documentResultSet
	if len(taskIDs) == 0 {
		markerRows, err = documentRows(ctx, s.db, markerQuery+` ORDER BY e.at,e.id`, documentWorkspace(ctx))
	} else {
		markerRows, err = documentBatchRows(ctx, s.db, markerQuery+` AND e.task_id IN (%s) ORDER BY e.at,e.id`, documentWorkspace(ctx), taskIDs)
	}

	if err != nil {
		return nil, err
	}
	for markerRows.Next() {
		var event core.Event
		if scanErr := markerRows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.Payload, &event.At); scanErr != nil {
			markerRows.Close()
			return nil, scanErr
		}
		switch event.Kind {
		case "work_order.claimed", "work_order.lease_renewed", "work_order.released", "review.completed", "review.accepted", "task.setup.changed":
			if hasReviewOrders {
				eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
			}
		}
		switch event.Kind {
		case "pipeline.bounced", "work_order.claimed":
			requestEventsByTask[event.TaskID] = append(requestEventsByTask[event.TaskID], event)
		case "github_issue.publication_failed", "github_issue.publication_published", "review.publication_failed", "review.publication_published", "merge.failed", "merge.confirmed", "merge.reconciled":
			forgeEventsByTask[event.TaskID] = append(forgeEventsByTask[event.TaskID], event)
		}
	}
	if err = markerRows.Err(); err != nil {
		markerRows.Close()
		return nil, err
	}
	markerRows.Close()
	taskIDsForState := make([]string, 0, len(rows))
	for _, row := range rows {
		taskIDsForState = append(taskIDsForState, row.taskID)
	}
	taskStates := make(map[string]core.TaskState, len(taskIDsForState))
	if len(taskIDsForState) > 0 {
		stateRows, stateErr := documentBatchRows(ctx, s.db, `SELECT id,state FROM tasks WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), taskIDsForState)
		if stateErr != nil {
			return nil, stateErr
		}
		for stateRows.Next() {
			var taskID string
			var state core.TaskState
			if stateErr = stateRows.Scan(&taskID, &state); stateErr != nil {
				stateRows.Close()
				return nil, stateErr
			}
			taskStates[taskID] = state
		}
		if stateErr = stateRows.Err(); stateErr != nil {
			stateRows.Close()
			return nil, stateErr
		}
		stateRows.Close()
	}
	result := make([]store.ActivityMarker, len(rows))
	for i, row := range rows {
		task := core.Task{ID: row.taskID, State: taskStates[row.taskID]}
		result[i] = store.ActivityMarker{
			TaskID: row.taskID, LatestStage: core.Stage(row.latestStage), LastEventAt: row.lastEventAt, LastEventID: row.lastEventID,
			ForgeFailure:              store.LatestForgeFailure(forgeEventsByTask[row.taskID]),
			ReviewDiagnostics:         store.ReviewVerdictDiagnostics(ordersByTask[row.taskID], eventsByTask[row.taskID], time.Now().UTC()),
			ReviewRecovery:            store.ReviewRecoveryNeeded(ordersByTask[row.taskID], eventsByTask[row.taskID]),
			InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(ordersByTask[row.taskID], eventsByTask[row.taskID]), eventsByTask[row.taskID]),
			Stalled:                   store.StalledTask(ordersByTask[row.taskID]),
			UserChangesRequested:      store.UserRequestChangesPending(requestEventsByTask[row.taskID]),
		}
	}
	return result, nil
}

func (s *Store) AppendEvent(ctx context.Context, e core.Event) error {
	return s.documentTx(ctx, func(tx *sql.Tx) error {
		if e.JobID != "" {
			var taskID string
			if err := documentRow(ctx, tx, `SELECT task_id FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), e.JobID).Scan(&taskID); err != nil || taskID != e.TaskID {
				return fmt.Errorf("job does not belong to event task in this workspace")
			}
		}
		return insertEvent(ctx, tx, e)
	})
}
func (s *Store) ListEvents(ctx context.Context, id string) ([]core.Event, error) {
	return documentTaskEvents(ctx, s.db, id)
}
func (s *Store) ListEventsAfter(ctx context.Context, id string, after int64) ([]core.Event, error) {
	events, err := s.ListEvents(ctx, id)
	out := []core.Event{}
	for _, e := range events {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, err
}
func (s *Store) CountEvents(ctx context.Context, id, kind string) (int, error) {
	var n int
	err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND task_id=? AND kind=?`, documentWorkspace(ctx), id, kind).Scan(&n)
	return n, err
}
func (s *Store) CountEventsSinceHumanIntervention(ctx context.Context, id, kind string) (int, error) {
	var n int
	err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM events e WHERE workspace_id=? AND task_id=? AND kind=? AND (NOT EXISTS(SELECT 1 FROM interventions i WHERE i.workspace_id=e.workspace_id AND i.task_id=e.task_id AND i.actor_role='human') OR e.at>(SELECT MAX(i.at) FROM interventions i WHERE i.workspace_id=e.workspace_id AND i.task_id=e.task_id AND i.actor_role='human'))`, documentWorkspace(ctx), id, kind).Scan(&n)
	return n, err
}
func (s *Store) ListRequirementEvents(ctx context.Context, id string) ([]core.Event, error) {
	all, err := s.ListRequirementEventsByRequirement(ctx)
	return append([]core.Event{}, all[id]...), err
}
func (s *Store) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,kind,actor_id,actor_role,payload_json,at,JSON_EXTRACT_STRING(payload_json,'requirement_id') FROM events WHERE workspace_id=? AND task_id IS NULL AND JSON_EXTRACT_STRING(payload_json,'requirement_id') IS NOT NULL ORDER BY at,id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.Event{}
	for rows.Next() {
		var e core.Event
		var id string
		if err = rows.Scan(&e.ID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At, &id); err != nil {
			return nil, err
		}
		out[id] = append(out[id], e)
	}
	return out, rows.Err()
}
func (s *Store) ListRequirementDeliveryEventsForTasks(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,task_id,COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND task_id IN (%s) AND kind IN ('merge.confirmed','merge.reconciled','review.round_completed','task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed') ORDER BY task_id,at,id`, documentWorkspace(ctx), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e core.Event
		if err = rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
			return nil, err
		}
		out[e.TaskID] = append(out[e.TaskID], e)
	}
	return out, rows.Err()
}
func (s *Store) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (store.DocumentEventPage, error) {
	page := store.DocumentEventPage{Events: []core.Event{}, Limit: q.Limit, Offset: q.Offset}
	if err := store.ValidateDocumentEventQuery(kind, q); err != nil {
		return page, err
	}
	// One statement pins count, snapshot, and page membership together. Only
	// selected event payloads cross the database boundary.
	rows, err := documentRows(ctx, s.db, `WITH matched AS (
 SELECT e.id,e.at FROM events e WHERE e.workspace_id=? AND (?=0 OR e.id<=?) AND (
 (?='requirement' AND ((e.task_id IS NULL AND JSON_EXTRACT_STRING(e.payload_json,'requirement_id')=?) OR EXISTS (
 SELECT 1 FROM task_context_proposals p WHERE p.workspace_id=e.workspace_id AND p.task_id=e.task_id AND p.target_kind='requirement' AND p.target_id=? AND p.state='confirmed')))
 OR (?='system_design' AND e.kind LIKE 'system_design.%' AND JSON_EXTRACT_STRING(e.payload_json,'document_id')=?))
 ), selected AS (SELECT id,at FROM matched ORDER BY at DESC,id DESC LIMIT ? OFFSET ?),
 totals AS (SELECT COUNT(*) AS total,COALESCE(MAX(id),0) AS snapshot FROM matched)
 SELECT totals.total,CASE WHEN ?>0 THEN ? ELSE totals.snapshot END,
 e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),COALESCE(e.kind,''),COALESCE(e.actor_id,''),COALESCE(e.actor_role,''),e.payload_json,e.at
 FROM totals LEFT JOIN selected p ON 1=1 LEFT JOIN events e ON e.workspace_id=? AND e.id=p.id ORDER BY e.at DESC,e.id DESC`,
		documentWorkspace(ctx), q.SnapshotID, q.SnapshotID, string(kind), id, id, string(kind), id, q.Limit, q.Offset, q.SnapshotID, q.SnapshotID, documentWorkspace(ctx))
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var e core.Event
		var eventID sql.NullInt64
		var payload []byte
		var at sql.NullTime
		if err = rows.Scan(&page.Total, &page.SnapshotID, &eventID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &payload, &at); err != nil {
			return page, err
		}
		if eventID.Valid {
			e.ID, e.At, e.Payload = eventID.Int64, at.Time, payload
			page.Events = append(page.Events, e)
		}
	}
	return page, rows.Err()
}
