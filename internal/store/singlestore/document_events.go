package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Document commands serialize their projections and event-derived lineage in
// one transaction (DEC-38; component-persistence; component-lineage).
func (s *Store) documentTx(ctx context.Context, fn func(*sql.Tx) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockDocumentCorpusTx(ctx, tx, ws); err != nil {
			return err
		}
		return fn(tx)
	})
}

// lockDocumentCorpusTx takes the workspace's document lock. A caller that also
// needs a task-operation lock takes that lock first (component-persistence,
// Lock order).
func lockDocumentCorpusTx(ctx context.Context, tx *sql.Tx, ws string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id=?`, ws).Scan(&exists); err != nil {
		return notFound(err, "workspace %s", ws)
	}
	return lockKey(ctx, tx, "document-corpus:"+ws)
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
			dest[i] = documentJSONStrings{p}
		}
	}
	return translateBackendConflict(r.documentScanner.Scan(dest...))
}

type documentJSONStrings struct{ value *[]string }

func (j documentJSONStrings) Scan(src any) error {
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

// Err and Close translate like Scan, so row iteration cannot return a driver
// error through a read outside a transaction (component-persistence).
func (r *documentResultSet) Err() error   { return translateBackendConflict(r.Rows.Err()) }
func (r *documentResultSet) Close() error { return translateBackendConflict(r.Rows.Close()) }
func documentRow(ctx context.Context, db s2log.Executor, query string, args ...any) documentScanner {
	if _, err := workspace(ctx); err != nil {
		return documentResult{err: err}
	}
	return backendRow(ctx, db, query, documentArgs(args)...)
}
func documentRows(ctx context.Context, db s2log.Executor, query string, args ...any) (*documentResultSet, error) {
	if _, err := workspace(ctx); err != nil {
		return nil, err
	}
	return backendRows(ctx, db, query, documentArgs(args)...)
}
func documentExec(ctx context.Context, db s2log.Executor, query string, args ...any) (sql.Result, error) {
	if _, err := workspace(ctx); err != nil {
		return nil, err
	}
	return backendExec(ctx, db, query, documentArgs(args)...)
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

// notFound names a missing row. Any other error is translated, so a read
// outside a transaction, such as a claim's order lookup, cannot return driver
// text (component-persistence).
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, fmt.Sprintf(format, args...))
	}
	return translateBackendConflict(err)
}
func insertLineageLink(ctx context.Context, tx *sql.Tx, l core.LineageLink) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE created_by_event_id=LEAST(COALESCE(created_by_event_id,VALUES(created_by_event_id)),VALUES(created_by_event_id)),created_at=LEAST(created_at,VALUES(created_at)),legacy_created_by_event=NULL`, l.Workspace, l.SrcType, l.SrcID, l.DstType, l.DstID, l.Kind, l.CreatedByEventID, l.CreatedAt)
	return err
}

func documentTaskEvents(ctx context.Context, db s2log.Executor, taskID string) ([]core.Event, error) {
	rows, err := documentRows(ctx, db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key ORDER BY at,id`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	return scanDocumentEvents(rows)
}

func scanDocumentEvents(rows *documentResultSet) ([]core.Event, error) {
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var e core.Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListActivityMarkers")
	markers, _, _, err := s.listActivityMarkers(ctx, nil, time.Now().UTC())
	return markers, err
}

func (s *Store) ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListActivityMarkersForTasks")
	if taskIDs != nil && len(taskIDs) == 0 {
		return []store.ActivityMarker{}, nil
	}
	markers, _, _, err := s.listActivityMarkers(ctx, taskIDs, time.Now().UTC())
	return markers, err
}

// The event kinds each activity-marker fold reads. listActivityMarkers
// selects only these kinds, and of them only the rows a fold can act on
// (store.MarkerFoldSQL): a bounce from user-request-changes, an implement
// claim or a claim of a task with an open review order, and the review
// lifecycle kinds of review tasks.
var (
	activityMarkerReviewKinds  = []string{"work_order.claimed", "work_order.lease_renewed", "work_order.released", "review.completed", "review.accepted", "task.setup.changed"}
	activityMarkerRequestKinds = []string{"pipeline.bounced", "work_order.claimed"}
	// The forge failure kinds are the ones store.LatestForgeFailure reads;
	// of work_order.updated the query selects only an update whose payload
	// may name the implement stage and the submitted state
	// (component-next-action; component-submission-merge).
	activityMarkerForgeKinds = store.ForgeFailureEventKinds
)

// listActivityMarkers folds the markers from one order selection projected
// at now and returns that selection and the review lease horizon of the
// lifecycle events it read (component-persistence, Board activity validation
// reads).
func (s *Store) listActivityMarkers(ctx context.Context, taskIDs []string, now time.Time) ([]store.ActivityMarker, []core.WorkOrder, time.Time, error) {
	rows, err := s.markerTaskRows(ctx, taskIDs)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	// Scope the order read to the requested tasks. The activity feed still
	// wants the whole workspace, but a Tasks page must not pull every
	// workspace order in only to discard most of it.
	if len(rows) == 0 {
		return []store.ActivityMarker{}, []core.WorkOrder{}, time.Time{}, nil
	}
	if err = s.markerLatestSummaries(ctx, rows); err != nil {
		return nil, nil, time.Time{}, err
	}
	orders, err := s.activityWorkOrdersAt(ctx, taskIDs, now)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	var dependencyTaskIDs []string
	seenDependencyTask := map[string]bool{}
	for _, order := range orders {
		if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !seenDependencyTask[order.TaskID] {
			dependencyTaskIDs = append(dependencyTaskIDs, order.TaskID)
			seenDependencyTask[order.TaskID] = true
		}
	}
	blockersByTask := map[string]store.DependencyBlockers{}
	if len(dependencyTaskIDs) > 0 {
		blockersByTask, err = s.ListDependencyBlockers(ctx, dependencyTaskIDs)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	reviewTaskIDs := make([]string, 0)
	seenReviewTask := map[string]bool{}
	var liveReviewTaskIDs []string
	seenLiveReviewTask := map[string]bool{}
	var openReviewOrders []store.MarkerReviewOrder
	for _, order := range orders {
		if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
			blockers := blockersByTask[order.TaskID]
			order.BlockingTaskIDs = append([]string(nil), blockers.BlockingTaskIDs...)
			order.UnsatisfiableTaskIDs = append([]string(nil), blockers.UnsatisfiableTaskIDs...)
			if len(order.BlockingTaskIDs) > 0 {
				order.Claimable = false
			}
		}
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
		if order.Stage == core.StageReview && !seenReviewTask[order.TaskID] {
			reviewTaskIDs = append(reviewTaskIDs, order.TaskID)
			seenReviewTask[order.TaskID] = true
		}
		// Only an open review order reads its claim and renewal events:
		// ReviewVerdictDiagnostics and ActivityReviewLeaseHorizon skip the rest.
		if order.Stage == core.StageReview && (order.State == core.WorkOrderClaimed || order.State == core.WorkOrderQueued) {
			openReviewOrders = append(openReviewOrders, store.MarkerReviewOrder{TaskID: order.TaskID, ID: order.ID, JobID: order.JobID})
			if !seenLiveReviewTask[order.TaskID] {
				liveReviewTaskIDs = append(liveReviewTaskIDs, order.TaskID)
				seenLiveReviewTask[order.TaskID] = true
			}
		}
	}
	pageTaskIDs := make([]string, len(rows))
	for i := range rows {
		pageTaskIDs[i] = rows[i].taskID
	}
	foldEvents, err := s.markerFoldRows(ctx, store.MarkerFoldInput{
		Workspace: documentWorkspace(ctx), Page: pageTaskIDs, ReviewTasks: reviewTaskIDs,
		LiveReviewTasks: liveReviewTaskIDs, OpenReviewOrders: openReviewOrders, Now: now,
	})
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	eventsByTask := make(map[string][]core.Event)
	requestEventsByTask := make(map[string][]core.Event)
	forgeEventsByTask := make(map[string][]core.Event)
	reviewTasks := make(map[string]bool, len(reviewTaskIDs))
	for _, id := range reviewTaskIDs {
		reviewTasks[id] = true
	}
	for _, event := range foldEvents {
		if reviewTasks[event.TaskID] && slices.Contains(activityMarkerReviewKinds, event.Kind) {
			eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
		}
		switch {
		case slices.Contains(activityMarkerRequestKinds, event.Kind):
			requestEventsByTask[event.TaskID] = append(requestEventsByTask[event.TaskID], event)
		case slices.Contains(activityMarkerForgeKinds, event.Kind):
			forgeEventsByTask[event.TaskID] = append(forgeEventsByTask[event.TaskID], event)
		}
	}
	var horizon time.Time
	for taskID, taskOrders := range ordersByTask {
		lease := store.ActivityReviewLeaseHorizon(taskOrders, eventsByTask[taskID], now)
		if !lease.IsZero() && (horizon.IsZero() || lease.Before(horizon)) {
			horizon = lease
		}
	}
	result := make([]store.ActivityMarker, len(rows))
	for i, row := range rows {
		task := core.Task{ID: row.taskID, State: row.state}
		result[i] = store.ActivityMarker{
			TaskID: row.taskID, LatestStage: core.Stage(row.latestStage), LastEventAt: row.lastEventAt, LastEventID: row.lastEventID,
			ForgeFailure:              store.LatestForgeFailure(forgeEventsByTask[row.taskID]),
			ReviewDiagnostics:         store.ReviewVerdictDiagnostics(ordersByTask[row.taskID], eventsByTask[row.taskID], now),
			ReviewRecovery:            store.ReviewRecoveryNeeded(ordersByTask[row.taskID], eventsByTask[row.taskID]),
			InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(ordersByTask[row.taskID], eventsByTask[row.taskID]), eventsByTask[row.taskID]),
			Stalled:                   store.StalledTask(ordersByTask[row.taskID]),
			UserChangesRequested:      store.UserRequestChangesPending(requestEventsByTask[row.taskID]),
		}
	}
	return result, orders, horizon, nil
}

func (s *Store) AppendEvent(ctx context.Context, e core.Event) error {
	ctx = dbmetrics.WithReadLabel(ctx, "AppendEvent")
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
	ctx = dbmetrics.WithReadLabel(ctx, "ListEvents")
	return documentTaskEvents(ctx, s.db, id)
}

// ListEventPage reads one page of ListEvents with its task-wide count. The
// count runs first, so a page past the end still states it.
func (s *Store) ListEventPage(ctx context.Context, id string, page store.PageQuery) (store.EventPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEventPage")
	if err := page.Validate(); err != nil {
		return store.EventPage{}, err
	}
	var total int
	if err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key`, documentWorkspace(ctx), id).Scan(&total); err != nil {
		return store.EventPage{}, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key ORDER BY at,id LIMIT ? OFFSET ?`, documentWorkspace(ctx), id, page.Limit, page.Offset)
	if err != nil {
		return store.EventPage{}, err
	}
	events, err := scanDocumentEvents(rows)
	if err != nil {
		return store.EventPage{}, err
	}
	return store.EventPage{Events: events, Total: total}, nil
}

// ListRecoveryActPage reads one page of the workspace's system recovery acts
// with the count the query selects (component-next-action invariant 21).
func (s *Store) ListRecoveryActPage(ctx context.Context, query store.RecoveryActQuery) (store.RecoveryActPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRecoveryActPage")
	if err := query.Page.Validate(); err != nil {
		return store.RecoveryActPage{}, err
	}
	kinds := store.RecoveryActEventKinds()
	selected := ` FROM events_columnstore WHERE workspace_id=? AND at>=? AND actor_role=? AND task_id IS NOT NULL AND kind IN (?` + strings.Repeat(`,?`, len(kinds)-1) + `)`
	args := []any{documentWorkspace(ctx), query.Since, string(core.ActorSystem)}
	for _, kind := range kinds {
		args = append(args, kind)
	}
	var total int
	if err := documentRow(ctx, s.db, `SELECT COUNT(*)`+selected, args...).Scan(&total); err != nil {
		return store.RecoveryActPage{}, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,task_id,kind,at`+selected+` ORDER BY at DESC,id DESC LIMIT ? OFFSET ?`, append(args, query.Page.Limit, query.Page.Offset)...)
	if err != nil {
		return store.RecoveryActPage{}, err
	}
	defer rows.Close()
	acts := []nextaction.Act{}
	for rows.Next() {
		var (
			id           int64
			taskID, kind string
			at           time.Time
		)
		if err = rows.Scan(&id, &taskID, &kind, &at); err != nil {
			return store.RecoveryActPage{}, err
		}
		acts = append(acts, store.RecoveryActFromEvent(id, taskID, kind, at.UTC()))
	}
	if err = rows.Err(); err != nil {
		return store.RecoveryActPage{}, err
	}
	return store.RecoveryActPage{Acts: acts, Total: total}, nil
}

func (s *Store) ListFactoryActs(ctx context.Context, since time.Time, limit int) ([]nextaction.Act, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFactoryActs")
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),kind,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND at>=? AND kind IN ('pipeline.bounced','work_order.expired','work_order.released','work_order.requeued','merge.conflict_fix_dispatched','review.refresh_round_created','review.seat_recovered','merge.confirmed','drift.repaired','merge.bypass_recorded') ORDER BY at DESC, id DESC LIMIT ?`, documentWorkspace(ctx), since, limit*8)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var event core.Event
		if err = rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.ActorRole, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return store.SelectFactoryActs(events, since, limit), nil
}

// ListEventsAfter resolves a nonzero anchor owned by the task in this
// workspace and returns events strictly after its (at, id) tuple. IDs come
// from per-aggregator AUTO_INCREMENT ranges, so numeric order is not insertion
// order and never selects the tail (component-persistence; DEC-39).
func (s *Store) ListEventsAfter(ctx context.Context, id string, after int64) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEventsAfter")
	query := `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key ORDER BY at,id`
	args := []any{documentWorkspace(ctx), id}
	if after != 0 {
		var anchorAt time.Time
		err := documentRow(ctx, s.db, `SELECT at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND id=?`, documentWorkspace(ctx), id, after).Scan(&anchorAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrEventAnchorNotFound
		}
		if err != nil {
			return nil, err
		}
		query = `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND (at>? OR at=? AND id>?) ORDER BY at,id`
		args = append(args, anchorAt, anchorAt, after)
	}
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	return scanDocumentEvents(rows)
}

// ReadTaskEventStream selects one bounded (at, id) page on the task timeline
// index with one lookahead row.
func (s *Store) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadTaskEventStream")
	if err := store.ValidateTaskEventStreamQuery(q); err != nil {
		return store.TaskEventStreamPage{}, err
	}
	var exists int
	if err := documentRow(ctx, s.db, `SELECT 1 FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), q.TaskID).Scan(&exists); err != nil {
		return store.TaskEventStreamPage{}, notFound(err, "task %s", q.TaskID)
	}
	query := `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key`
	args := []any{documentWorkspace(ctx), q.TaskID}
	if !q.Since.IsZero() {
		query += ` AND at>=?`
		args = append(args, q.Since.UTC())
	}
	if q.After != nil {
		query += ` AND (at>? OR at=? AND id>?)`
		args = append(args, q.After.At.UTC(), q.After.At.UTC(), q.After.ID)
	}
	query += ` ORDER BY at,id LIMIT ?`
	args = append(args, q.Limit+1)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return store.TaskEventStreamPage{}, err
	}
	events, err := scanDocumentEvents(rows)
	if err != nil {
		return store.TaskEventStreamPage{}, err
	}
	page := store.TaskEventStreamPage{Events: events}
	if len(events) > q.Limit {
		page.Events, page.More = events[:q.Limit], true
	}
	return page, nil
}
func (s *Store) CountEvents(ctx context.Context, id, kind string) (int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CountEvents")
	var n int
	err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND kind=?`, documentWorkspace(ctx), id, kind).Scan(&n)
	return n, err
}
func (s *Store) CountEventsSinceHumanIntervention(ctx context.Context, id, kind string) (int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CountEventsSinceHumanIntervention")
	var n int
	err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM events_columnstore e
		LEFT JOIN (SELECT MAX(at) AS last_at FROM interventions WHERE workspace_id=? AND task_id=? AND actor_role='human') i ON 1=1
		WHERE e.workspace_id=? AND e.event_shard_key=? AND e.task_id=e.event_shard_key AND e.kind=? AND (i.last_at IS NULL OR e.at>i.last_at)`, documentWorkspace(ctx), id, documentWorkspace(ctx), id, kind).Scan(&n)
	return n, err
}
func (s *Store) ListRequirementEvents(ctx context.Context, id string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementEvents")
	all, err := s.ListRequirementEventsByRequirement(ctx)
	return append([]core.Event{}, all[id]...), err
}
func (s *Store) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementEventsByRequirement")
	rows, err := documentRows(ctx, s.db, `SELECT id,kind,actor_id,actor_role,payload_json,at,JSON_EXTRACT_STRING(payload_json,'requirement_id') FROM events_columnstore WHERE workspace_id=? AND task_id IS NULL AND JSON_EXTRACT_STRING(payload_json,'requirement_id') IS NOT NULL ORDER BY at,id`, documentWorkspace(ctx))
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
func (s *Store) ListRequirementEventsForRequirements(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementEventsForRequirements")
	out := make(map[string][]core.Event, len(ids))
	predicates := make([]string, 0, len(ids))
	args := []any{documentWorkspace(ctx)}
	prepared := s.hotReadIndexReady("events_requirement_key_idx")
	for _, id := range ids {
		if _, exists := out[id]; exists {
			continue
		}
		out[id] = []core.Event{}
		if id == "" {
			continue
		}
		if prepared {
			predicates = append(predicates, "(event_requirement_key=SHA2(?,256) AND BINARY JSON_EXTRACT_STRING(payload_json,'requirement_id')=BINARY ?)")
			args = append(args, id, id)
		} else {
			predicates = append(predicates, "BINARY JSON_EXTRACT_STRING(payload_json,'requirement_id')=BINARY ?")
			args = append(args, id)
		}
	}
	if len(predicates) == 0 {
		return out, nil
	}
	query := `SELECT id,kind,actor_id,actor_role,payload_json,at,JSON_EXTRACT_STRING(payload_json,'requirement_id') FROM events_columnstore WHERE workspace_id=? AND task_id IS NULL AND (` + strings.Join(predicates, " OR ") + `) ORDER BY at,id`
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var event core.Event
		var id string
		if err := rows.Scan(&event.ID, &event.Kind, &event.ActorID, &event.ActorRole, &event.Payload, &event.At, &id); err != nil {
			return nil, err
		}
		if _, ok := out[id]; ok {
			out[id] = append(out[id], event)
		}
	}
	return out, rows.Err()
}
func (s *Store) ListRequirementDeliveryEventsForTasks(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementDeliveryEventsForTasks")
	out := map[string][]core.Event{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,task_id,COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND task_id IN (%s) AND kind IN ('merge.confirmed','merge.reconciled','review.round_completed','task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed') ORDER BY task_id,at,id`, documentWorkspace(ctx), ids)
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
func (s *Store) ListMonitorPullRequestEventsForTasks(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListMonitorPullRequestEventsForTasks")
	out := map[string][]core.Event{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,task_id,COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND task_id IN (%s) AND kind='pull_request.opened' ORDER BY task_id,at,id`, documentWorkspace(ctx), ids)
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
func (s *Store) ListLatestPullRequestEventsForTasks(ctx context.Context, ids []string) (map[string]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListLatestPullRequestEventsForTasks")
	out := make(map[string]core.Event, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,task_id,job_id,kind,actor_id,actor_role,payload_json,at FROM (SELECT id,task_id,COALESCE(job_id,'') AS job_id,kind,actor_id,actor_role,payload_json,at,ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY at DESC,id DESC) AS rn FROM events_columnstore WHERE workspace_id=? AND task_id IN (%s) AND kind='pull_request.opened') ranked WHERE rn=1`, documentWorkspace(ctx), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e core.Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
			return nil, err
		}
		out[e.TaskID] = e
	}
	return out, rows.Err()
}
func (s *Store) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (store.DocumentEventPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListDocumentEventPage")
	page := store.DocumentEventPage{Events: []core.Event{}, Limit: q.Limit, Offset: q.Offset}
	if err := store.ValidateDocumentEventQuery(kind, q); err != nil {
		return page, err
	}
	// One statement pins count, snapshot, and page membership together. Only
	// selected event payloads cross the database boundary.
	query, args := documentEventPageQueryForLayout(documentWorkspace(ctx), kind, id, q, s.hotReadIndexReady("events_requirement_key_idx"))
	return readDocumentEventPage(ctx, s.db, page, query, args...)
}

// readDocumentEventPage decodes the totals row and the selected page rows of
// one document event page statement.
func readDocumentEventPage(ctx context.Context, db s2log.Executor, page store.DocumentEventPage, query string, args ...any) (store.DocumentEventPage, error) {
	rows, err := documentRows(ctx, db, query, args...)
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

// documentEventPageQuery selects the membership statement by document kind in
// Go instead of one OR predicate over both kinds (component-persistence;
// component-document-corpus, Public surfaces).
func documentEventPageQuery(ws string, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (string, []any) {
	return documentEventPageQueryForLayout(ws, kind, id, q, false)
}

func documentEventPageQueryForLayout(ws string, kind core.LineageNodeType, id string, q store.DocumentEventQuery, prepared bool) (string, []any) {
	page := []any{q.Limit, q.Offset, q.SnapshotID, q.SnapshotID, ws}
	if kind == core.LineageSystemDesign {
		return systemDesignDocumentEventPageSQL, append([]any{ws, id, q.SnapshotID, q.SnapshotID}, page...)
	}
	if prepared {
		query := strings.Replace(requirementDocumentEventPageSQL, "JSON_EXTRACT_STRING(e.payload_json,'requirement_id')=?", "e.event_requirement_key=SHA2(?,256) AND JSON_EXTRACT_STRING(e.payload_json,'requirement_id')=?", 1)
		return query, append([]any{ws, id, id, q.SnapshotID, q.SnapshotID, ws, id, q.SnapshotID, q.SnapshotID}, page...)
	}
	return requirementDocumentEventPageSQL, append([]any{ws, id, q.SnapshotID, q.SnapshotID, ws, id, q.SnapshotID, q.SnapshotID}, page...)
}

// The requirement membership is the union of two branches. Events of a task
// are reached from the confirmed context proposals by task ID. event_shard_key
// equals task_id for every task event, so this branch reads only the matching
// tasks' events. Events without a task are filtered by the payload's
// requirement_id within the workspace. task_id IS NULL does not bound that
// scan, and no key covers the payload, so this branch reads every segment
// of the workspace.
const requirementDocumentEventPageSQL = `WITH matched AS (
 SELECT e.id,e.at,e.event_shard_key FROM events_columnstore e
 WHERE e.workspace_id=? AND e.task_id IS NULL AND JSON_EXTRACT_STRING(e.payload_json,'requirement_id')=? AND (?=0 OR e.id<=?)
 UNION
 SELECT e.id,e.at,e.event_shard_key FROM task_context_proposals p
 JOIN events_columnstore e ON e.workspace_id=p.workspace_id AND e.event_shard_key=p.task_id AND e.task_id=p.task_id
 WHERE p.workspace_id=? AND p.target_kind='requirement' AND p.target_id=? AND p.state='confirmed' AND (?=0 OR e.id<=?)
` + documentEventPageSQL

// System Design history includes task-associated consultation events, so this
// branch keeps no task_id condition. No key covers kind or the payload's
// document_id, so it reads every segment of the workspace.
const systemDesignDocumentEventPageSQL = `WITH matched AS (
 SELECT e.id,e.at,e.event_shard_key FROM events_columnstore e
 WHERE e.workspace_id=? AND e.kind LIKE 'system_design.%' AND JSON_EXTRACT_STRING(e.payload_json,'document_id')=? AND (?=0 OR e.id<=?)
` + documentEventPageSQL

// The page fetch reads only the selected rows. It uses each row's stored
// event_shard_key, never one derived from task_id, because an event without a
// task has its own shard key. The workspace, shard key, and ID equalities use
// the unique hash key events_task_event_idx. A join on the workspace and ID
// alone repartitions every event of the workspace.
const documentEventPageSQL = ` ), selected AS (SELECT id,at,event_shard_key FROM matched ORDER BY at DESC,id DESC LIMIT ? OFFSET ?),
 totals AS (SELECT COUNT(*) AS total,COALESCE(MAX(id),0) AS snapshot FROM matched)
 SELECT totals.total,CASE WHEN ?>0 THEN ? ELSE totals.snapshot END,
 e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),COALESCE(e.kind,''),COALESCE(e.actor_id,''),COALESCE(e.actor_role,''),e.payload_json,e.at
 FROM totals LEFT JOIN selected p ON 1=1 LEFT JOIN events_columnstore e ON e.workspace_id=? AND e.event_shard_key=p.event_shard_key AND e.id=p.id ORDER BY e.at DESC,e.id DESC`
