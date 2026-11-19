package singlestore

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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
	page, fixture := storetest.SeedActivityMarkerEgress(t, ctx, st, documentWorkspace(ctx), now)
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

// legacyListActivityMarkers and the functions below are the activity-marker
// read at origin/main 3062eb520, the measurement baseline. Do not edit them
// to follow the store.
func legacyListActivityMarkers(s *Store, ctx context.Context, taskIDs []string, now time.Time) ([]store.ActivityMarker, []core.WorkOrder, time.Time, error) {
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
	foldEvents, err := legacyMarkerFoldRows(s, ctx, pageTaskIDs, reviewTaskIDs, liveReviewTaskIDs, now)
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

func legacyMarkerKindLiterals(kinds []string) string {
	quoted := make([]string, len(kinds))
	for i, kind := range kinds {
		quoted[i] = "'" + strings.ReplaceAll(kind, "'", "''") + "'"
	}
	return strings.Join(quoted, ",")
}

// legacyMarkerForgePageKinds are the forge failure kinds read for the page's
// tasks. work_order.updated is read separately, only for an implement
// order's submission, exactly as store.LatestForgeFailure reads it.
func legacyMarkerForgePageKinds() []string {
	kinds := make([]string, 0, len(activityMarkerForgeKinds))
	for _, kind := range activityMarkerForgeKinds {
		if kind != "work_order.updated" {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// legacyMarkerLifecycleKinds are the event kinds the review lease folds read: the
// claim that opens a review lease and the renewals that extend it.
const legacyMarkerLifecycleKinds = `'work_order.claimed','work_order.lease_renewed'`

// legacyMarkerFoldQuery retains the last valid renewal for diagnostics and the
// earliest future renewal for the validator's lease horizon. Only the simple
// renewal payload written by the store is eligible: other JSON shapes retain
// their complete history for the Go decoder (including duplicate/escaped keys,
// payload identities, malformed leases, and whole-payload decoding errors).
func legacyMarkerFoldQuery(ws string, pageTaskIDs, reviewTaskIDs, liveReviewTaskIDs []string, now time.Time) (string, []any) {
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
	query := `SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + compactEventPayloadColumn + ` AS event_payload,e.at AS event_at
		FROM events_columnstore e
		WHERE e.workspace_id=? AND e.task_id IS NOT NULL AND e.task_id IN (` + taskMapMarks(len(pageTaskIDs)) + `)
		AND (
			e.kind IN ('pipeline.bounced','work_order.claimed','work_order.updated') OR
			(e.kind IN (` + legacyMarkerKindLiterals(legacyMarkerForgePageKinds()) + `))` + reviewBranch + liveBranch + `
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
	instants := markerLeaseInstantSQL(`SELECT `+carry+`,
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
func legacyMarkerFoldRows(s *Store, ctx context.Context, pageTaskIDs, reviewTaskIDs, liveReviewTaskIDs []string, now time.Time) ([]core.Event, error) {
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
		query, args := legacyMarkerFoldQuery(documentWorkspace(ctx), chunk, review, live, now)
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
