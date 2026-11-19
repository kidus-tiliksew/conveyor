package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// buildActivityMarkers compares the activity-marker read of origin/main
// 3062eb520, copied inline below, with the public ReadActivityInputs on one
// page of tasks with long claim, update, and bounce histories. Both serve
// every task's markers and the page's review lease horizon at one instant.
func buildActivityMarkers(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	page, fixture := storetest.SeedActivityMarkerEgress(t, ctx, st, workspace(ctx), now)
	if _, err := st.pool.Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatal(err)
	}
	return storetest.ReadEgressCase{Name: "activity-markers", Fixture: fixture,
		BaselineRevision: storetest.ActivityMarkerEgressBaselineRevision,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			markers, _, horizon, err := legacyListActivityMarkers(st, dbmetrics.WithReadLabel(ctx, "LegacyActivityMarkers"), page, now)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return storetest.ActivityMarkerEgressOutcome(markers, horizon)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			inputs, err := st.ReadActivityInputs(ctx, page, now)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return storetest.ActivityMarkerEgressOutcome(inputs.Markers, inputs.ValidUntil)
		}}
}

// legacyListActivityMarkers is listActivityMarkers at origin/main 3062eb520,
// the measurement baseline. Do not edit it to follow the store.
func legacyListActivityMarkers(s *Store, ctx context.Context, taskIDs []string, now time.Time) ([]store.ActivityMarker, []core.WorkOrder, time.Time, error) {
	type markerRow struct {
		taskID      string
		state       core.TaskState
		latestStage string
		lastEventAt time.Time
		lastEventID int64
	}
	var rows []markerRow
	var selected pgx.Rows
	var err error
	if len(taskIDs) == 0 {
		selected, err = s.boundary.Query(ctx, `SELECT t.id,
			COALESCE(
				(SELECT w.stage FROM work_orders w WHERE w.workspace_id=t.workspace_id AND w.task_id=t.id
					AND w.state='claimed'
					ORDER BY w.execution_started_at DESC NULLS LAST,w.created_at DESC,w.id DESC LIMIT 1),
				(SELECT j.stage FROM jobs j WHERE j.task_id=t.id ORDER BY j.started_at DESC,j.id DESC LIMIT 1),
				''
			)::text,
			COALESCE(e.at,t.created_at)::timestamptz,
			COALESCE(e.id,0)::bigint,
			t.state::text
			FROM tasks t
			LEFT JOIN LATERAL (
				SELECT e.at,e.id FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC,e.id DESC LIMIT 1
			) e ON true
			WHERE t.workspace_id=$1
			ORDER BY t.created_at,t.id`, workspace(ctx))
	} else {
		selected, err = s.boundary.Query(ctx, `SELECT t.id,
			COALESCE(
				(SELECT w.stage FROM work_orders w WHERE w.workspace_id=t.workspace_id AND w.task_id=t.id
					AND w.task_id=ANY($2::text[]) AND w.state='claimed'
					ORDER BY w.execution_started_at DESC NULLS LAST,w.created_at DESC,w.id DESC LIMIT 1),
				(SELECT j.stage FROM jobs j WHERE j.task_id=t.id ORDER BY j.started_at DESC,j.id DESC LIMIT 1),
				''
			)::text,
			COALESCE(e.at,t.created_at)::timestamptz,
			COALESCE(e.id,0)::bigint,
			t.state::text
			FROM tasks t
			LEFT JOIN LATERAL (
				SELECT e.at,e.id FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC,e.id DESC LIMIT 1
			) e ON true
			WHERE t.workspace_id=$1 AND t.id=ANY($2::text[])
			ORDER BY t.created_at,t.id`, workspace(ctx), taskIDs)
	}
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	for selected.Next() {
		var row markerRow
		if err = selected.Scan(&row.taskID, &row.latestStage, &row.lastEventAt, &row.lastEventID, &row.state); err != nil {
			selected.Close()
			return nil, nil, time.Time{}, err
		}
		rows = append(rows, row)
	}
	if err = selected.Err(); err != nil {
		selected.Close()
		return nil, nil, time.Time{}, err
	}
	selected.Close()
	// Scope the order read to the requested tasks. The activity feed still
	// wants the whole workspace, but a Tasks page must not pull every
	// workspace order in only to discard most of it.
	orders, err := s.activityWorkOrdersAt(ctx, taskIDs, now)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	var implementTaskIDs []string
	seenImplementTask := map[string]bool{}
	for _, order := range orders {
		if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !seenImplementTask[order.TaskID] {
			implementTaskIDs = append(implementTaskIDs, order.TaskID)
			seenImplementTask[order.TaskID] = true
		}
	}
	blockersByTask, err := s.ListDependencyBlockers(ctx, implementTaskIDs)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	hasReviewOrders := false
	reviewTaskIDs := make([]string, 0)
	seenReviewTask := map[string]bool{}
	var liveReviewTaskIDs []string
	seenLiveReviewTask := map[string]bool{}
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
		hasReviewOrders = hasReviewOrders || order.Stage == core.StageReview
		if order.Stage == core.StageReview && !seenReviewTask[order.TaskID] {
			reviewTaskIDs = append(reviewTaskIDs, order.TaskID)
			seenReviewTask[order.TaskID] = true
		}
		// Only an open review order reads its claim and renewal events:
		// ReviewVerdictDiagnostics and ActivityReviewLeaseHorizon skip the rest.
		if order.Stage == core.StageReview && (order.State == core.WorkOrderClaimed || order.State == core.WorkOrderQueued) {
			if !seenLiveReviewTask[order.TaskID] {
				liveReviewTaskIDs = append(liveReviewTaskIDs, order.TaskID)
				seenLiveReviewTask[order.TaskID] = true
			}
		}
	}
	eventsByTask := make(map[string][]core.Event)
	requestEventsByTask := make(map[string][]core.Event)
	forgeEventsByTask := make(map[string][]core.Event)
	// Keep Go decoding for fold semantics, reducing only safe renewal shapes.
	markerQuery := `SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + compactEventPayloadColumn + ` AS event_payload,e.at AS event_at
		FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1
		AND (cardinality($2::text[])=0 OR t.id=ANY($2::text[]))
		AND (
			e.kind IN ('pipeline.bounced','work_order.claimed','work_order.updated') OR
			(e.kind IN ('work_order.released','review.completed','review.accepted','task.setup.changed') AND e.task_id=ANY($3::text[])) OR
			(e.kind IN ('work_order.claimed','work_order.lease_renewed') AND e.task_id=ANY($5::text[])) OR
			e.kind=ANY($4::text[])
		)
		ORDER BY event_at,id`
	// The forge failure kinds are the ones store.LatestForgeFailure reads;
	// of work_order.updated it reads only an implement submission
	// (component-next-action; component-submission-merge).
	markerArgs := []any{workspace(ctx), emptyIfNil(taskIDs), emptyIfNil(reviewTaskIDs), store.ForgeFailureEventKinds, emptyIfNil(liveReviewTaskIDs)}
	if len(liveReviewTaskIDs) > 0 {
		markerQuery = legacyBoundedMarkerFoldQuery(markerQuery)
		markerArgs = append(markerArgs, now.Unix(), now.Nanosecond())
	}
	markerRows, err := s.boundary.Query(ctx, markerQuery, markerArgs...)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	for markerRows.Next() {
		var event core.Event
		if scanErr := markerRows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.Payload, &event.At); scanErr != nil {
			markerRows.Close()
			return nil, nil, time.Time{}, scanErr
		}
		switch event.Kind {
		case "work_order.claimed", "work_order.lease_renewed", "work_order.released", "review.completed", "review.accepted", "task.setup.changed":
			if hasReviewOrders {
				eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
			}
		}
		switch {
		case event.Kind == "pipeline.bounced" || event.Kind == "work_order.claimed":
			requestEventsByTask[event.TaskID] = append(requestEventsByTask[event.TaskID], event)
		case slices.Contains(store.ForgeFailureEventKinds, event.Kind):
			forgeEventsByTask[event.TaskID] = append(forgeEventsByTask[event.TaskID], event)
		}
	}
	if err = markerRows.Err(); err != nil {
		markerRows.Close()
		return nil, nil, time.Time{}, err
	}
	markerRows.Close()
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

// legacyBoundedMarkerFoldQuery is boundedMarkerFoldQuery at origin/main 3062eb520.
func legacyBoundedMarkerFoldQuery(query string) string {
	const attemptField = `"attempt_id"[[:space:]]*:[[:space:]]*"[^"]*"`
	const leaseField = `"lease_expires_at"[[:space:]]*:[[:space:]]*"[0-9T:Z+.,-]+"`
	const comma = `[[:space:]]*,[[:space:]]*`
	const renewalShape = `^[[:space:]]*[{][[:space:]]*(` + attemptField + comma + leaseField + `|` + leaseField + `(` + comma + attemptField + `){0,1})[[:space:]]*[}][[:space:]]*$`
	const carry = "id,task_id,job_id,event_payload,event_at"
	instants := markerLeaseInstantSQL(`SELECT `+carry+`,
		event_payload->>'lease_expires_at' AS lease_text,
		CASE WHEN event_payload::text ~ '`+renewalShape+`'
			AND jsonb_typeof(event_payload->'lease_expires_at')='string'
			THEN 1 ELSE 0 END AS lease_string
		FROM marker_rows WHERE kind='work_order.lease_renewed'`, carry)
	const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
	return `WITH marker_rows AS (` + query + `),
		marker_clock AS (SELECT $6::bigint AS now_sec,$7::bigint AS now_nanos),
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
}
