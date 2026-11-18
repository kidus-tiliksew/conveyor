package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunActivityMarkerBoundsConformance proves the activity-marker read folds a
// bounded window of a task's marker events into the markers the task's full
// event history produces, and that the rows it transfers do not grow with that
// history (component-task-lifecycle, Compact event reads;
// component-persistence, Database read accounting).
//
// One task holds a short renewal history and one holds five thousand
// renewals; every other marker kind is seeded identically on both. Each
// task's marker must equal the fold over its complete event list, and the
// five-thousand-renewal read must transfer no more rows than the short one.
func RunActivityMarkerBoundsConformance(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.SystemActor())
	// Lease instants sit a half hour either side of this instant, so the fold
	// compares them far from the moment of the read.
	now := time.Now().UTC().Truncate(time.Microsecond)

	const renewals = 5000
	shortTask := seedMarkerHistory(t, ctx, st, ws, now, "short", 8)
	longTask := seedMarkerHistory(t, ctx, st, ws, now, "long", renewals)

	markers := func(taskID string) store.ActivityMarker {
		t.Helper()
		read, err := st.ListActivityMarkersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		if len(read) != 1 || read[0].TaskID != taskID {
			t.Fatalf("ListActivityMarkersForTasks(%s) = %+v, want one marker", taskID, read)
		}
		return read[0]
	}
	// compare proves the read's markers are the ones the unbounded fold of the
	// task's complete event list derives. The stage and last-event fields come
	// from the bounded projection query, not from the marker event window, so
	// they stay outside this comparison.
	compare := func(taskID string, got store.ActivityMarker) {
		t.Helper()
		want := markerReference(t, ctx, st, taskID, now)
		requireSameJSON(t, "marker "+taskID+" forge failure", got.ForgeFailure, want.ForgeFailure)
		requireSameJSON(t, "marker "+taskID+" review diagnostics", got.ReviewDiagnostics, want.ReviewDiagnostics)
		requireSameJSON(t, "marker "+taskID+" review recovery", got.ReviewRecovery, want.ReviewRecovery)
		requireSameJSON(t, "marker "+taskID+" interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
		requireSameJSON(t, "marker "+taskID+" stalled", got.Stalled, want.Stalled)
		if got.UserChangesRequested != want.UserChangesRequested {
			t.Fatalf("marker %s user changes requested = %t, want %t", taskID, got.UserChangesRequested, want.UserChangesRequested)
		}
	}
	shortMarker, longMarker := markers(shortTask), markers(longTask)
	compare(shortTask, shortMarker)
	compare(longTask, longMarker)

	// The renewal history must reach the fold: the review diagnostic carries
	// the lease of the newest renewal, not the claim's lease.
	if len(longMarker.ReviewDiagnostics) == 0 {
		t.Fatal("the seeded review claim produced no review diagnostic")
	}
	if got, want := longMarker.ReviewDiagnostics[0].LeaseExpiresAt, now.Add(-30*time.Minute); !got.Equal(want) {
		t.Fatalf("review diagnostic lease = %s, want the newest renewal's %s", got, want)
	}

	// The restored selection, the claim reduction, and the payload projection
	// keep every marker and the review lease horizon of a mixed page.
	t.Run("predicates", func(t *testing.T) { runActivityMarkerPredicates(t, x, ctx, now) })

	if x.DrainReadRows == nil {
		t.Log("factory reports no read rows; the bounded-row assertion is skipped")
		return
	}
	// Rows per marker read must not follow the renewal history: the long
	// task's read may not transfer more rows than the short task's read.
	x.DrainReadRows()
	markers(longTask)
	longRows := x.DrainReadRows()["ListActivityMarkersForTasks"]
	x.DrainReadRows()
	markers(shortTask)
	shortRows := x.DrainReadRows()["ListActivityMarkersForTasks"]
	if shortRows == 0 || longRows == 0 {
		t.Fatalf("marker read rows short=%d long=%d, want both above zero", shortRows, longRows)
	}
	if longRows > shortRows {
		t.Fatalf("the %d-renewal marker read transferred %d rows, want no more than the %d-renewal read's %d", renewals, longRows, 8, shortRows)
	}
	// An unbounded read transfers one row per renewal, so this bound fails on
	// the shape it replaces.
	if longRows > 64 {
		t.Fatalf("the %d-renewal marker read transferred %d rows, want at most 64", renewals, longRows)
	}
}

// markerReference folds the markers of one task from every event it stored, the
// way the in-memory reference backend does, so a bounded read has something to
// be equal to.
func markerReference(t *testing.T, ctx context.Context, st store.Backend, taskID string, now time.Time) store.ActivityMarker {
	t.Helper()
	orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
	requireOK(t, err)
	blockers, err := st.ListDependencyBlockers(ctx, []string{taskID})
	requireOK(t, err)
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	task, err := st.GetTask(ctx, taskID)
	requireOK(t, err)
	for index := range orders {
		if orders[index].Stage != core.StageImplement && orders[index].Stage != core.StageVerify {
			continue
		}
		blocked := blockers[orders[index].TaskID]
		orders[index].BlockingTaskIDs = append([]string(nil), blocked.BlockingTaskIDs...)
		orders[index].UnsatisfiableTaskIDs = append([]string(nil), blocked.UnsatisfiableTaskIDs...)
		if len(orders[index].BlockingTaskIDs) > 0 {
			orders[index].Claimable = false
		}
	}
	return store.ActivityMarker{
		TaskID:                    taskID,
		ForgeFailure:              store.LatestForgeFailure(events),
		ReviewDiagnostics:         store.ReviewVerdictDiagnostics(orders, events, now),
		ReviewRecovery:            store.ReviewRecoveryNeeded(orders, events),
		InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(orders, events), events),
		Stalled:                   store.StalledTask(orders),
		UserChangesRequested:      store.UserRequestChangesPending(events),
	}
}

// seedMarkerHistory stores one task whose marker kinds exercise every fold the
// activity read feeds, with renewals lease_renewed events of a single review
// claim. The shape is identical for every renewal count.
func seedMarkerHistory(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, suffix string, renewals int) string {
	t.Helper()
	taskID := core.NewTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + taskID, Title: "Marker bounds " + suffix, Body: "marker bounds",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now.Add(-2 * time.Hour),
	}))
	newOrder := func(orderID string, stage core.Stage, state core.WorkOrderState, mutate func(*core.WorkOrder)) core.WorkOrder {
		t.Helper()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: stage, State: core.JobPending}))
		order := core.WorkOrder{
			ID: orderID, TaskID: taskID, JobID: orderID, Stage: stage, State: state,
			QueueEnteredAt: now.Add(-90 * time.Minute), QueueDeadline: now.Add(24 * time.Hour),
		}
		if mutate != nil {
			mutate(&order)
		}
		requireOK(t, CreateWorkOrder(ctx, st, order))
		return order
	}
	// The implement order claims after a user bounce, so the bounce marker
	// clears; a later bounce raises it again.
	implementID := taskID + "-implement-1"
	implementOrder := newOrder(implementID, core.StageImplement, core.WorkOrderQueued, func(order *core.WorkOrder) {
		order.ExecutionStartedAt = now.Add(-80 * time.Minute)
	})
	// The review order keeps a lapsed claim: its newest renewal is still in the
	// past, so the fold reports a claim without a verdict.
	reviewID := taskID + "-review-1"
	newOrder(reviewID, core.StageReview, core.WorkOrderQueued, func(order *core.WorkOrder) {
		order.ExecutionStartedAt = now.Add(-70 * time.Minute)
	})
	// The superseded seat is retry-suppressed and eligible on its own; the setup
	// change that supersedes it is what keeps the recovery marker clear.
	supersededID := taskID + "-review-2"
	newOrder(supersededID, core.StageReview, core.WorkOrderQueued, func(order *core.WorkOrder) {
		order.ExecutionStartedAt = now.Add(-60 * time.Minute)
		order.RetrySuppressed = true
		order.RetrySuppressionReason = core.AutomaticRecoveryExhaustedReason
	})
	appendEvent := func(event core.Event) {
		t.Helper()
		event.TaskID = taskID
		event.At = event.At.UTC()
		requireOK(t, st.AppendEvent(ctx, event))
	}
	payload := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		requireOK(t, err)
		return encoded
	}

	appendEvent(core.Event{Kind: "pipeline.bounced", At: now.Add(-85 * time.Minute), Payload: payload(map[string]any{"source": store.UserRequestChangesSource})})
	appendEvent(core.Event{JobID: implementID, Kind: "work_order.claimed", At: now.Add(-80 * time.Minute), Payload: store.WorkOrderEventPayload(implementOrder)})
	appendEvent(core.Event{Kind: "work_order.updated", At: now.Add(-79 * time.Minute), Payload: payload(map[string]any{"id": implementID, "stage": string(core.StageImplement), "state": string(core.WorkOrderSubmitted), "head_sha": "head-1"})})
	appendEvent(core.Event{JobID: reviewID, Kind: "work_order.claimed", At: now.Add(-70 * time.Minute), Payload: payload(map[string]any{"id": reviewID, "stage": string(core.StageReview), "state": string(core.WorkOrderClaimed), "lease_expires_at": now.Add(-65 * time.Minute)})})
	appendEvent(core.Event{JobID: reviewID, Kind: "work_order.released", At: now.Add(-69 * time.Minute), Payload: payload(map[string]any{"attempt_id": "attempt-0"})})
	appendEvent(core.Event{JobID: reviewID, Kind: "work_order.claimed", At: now.Add(-68 * time.Minute), Payload: payload(map[string]any{"id": reviewID, "stage": string(core.StageReview), "state": string(core.WorkOrderClaimed), "lease_expires_at": now.Add(-60 * time.Minute)})})
	// The renewals climb from the claim's lease to exactly half an hour ago;
	// every renewal shares one instant, so the window stays in the past for any
	// count while the fold orders them by (at, id) and reads the newest lease.
	leaseStep := 29 * time.Minute / time.Duration(renewals)
	for renewal := range renewals {
		lease := now.Add(-59*time.Minute + time.Duration(renewal+1)*leaseStep)
		appendEvent(core.Event{JobID: reviewID, Kind: "work_order.lease_renewed", At: now.Add(-66 * time.Minute), Payload: payload(map[string]any{"lease_expires_at": lease, "attempt_id": "attempt-1"})})
	}
	appendEvent(core.Event{JobID: supersededID, Kind: "review.completed", At: now.Add(-55 * time.Minute), Payload: payload(map[string]any{"review_work_order_id": supersededID, "review_round": 1, "review_seat": 2, "verdict": "approve", "reason_code": "approved"})})
	appendEvent(core.Event{Kind: "task.setup.changed", At: now.Add(-50 * time.Minute), Payload: payload(map[string]any{"review_transition": map[string]any{"superseded_work_order_ids": []string{supersededID}}})})
	appendEvent(core.Event{Kind: "github_issue.publication_retry", At: now.Add(-45 * time.Minute), Payload: payload(map[string]any{"automatic": true})})
	appendEvent(core.Event{Kind: "github_issue.publication_retry", At: now.Add(-44 * time.Minute), Payload: payload(map[string]any{"automatic": false})})
	appendEvent(core.Event{Kind: "github_issue.publication_failed", At: now.Add(-43 * time.Minute), Payload: payload(map[string]any{"forge_error_category": "issue_failed", "last_error": "issue publication failed"})})
	appendEvent(core.Event{JobID: reviewID, Kind: "review.publication_failed", At: now.Add(-42 * time.Minute), Payload: payload(map[string]any{"review_work_order_id": reviewID, "forge_error_category": "review_failed", "last_error": "review publication failed"})})
	appendEvent(core.Event{Kind: "merge.failed", At: now.Add(-40 * time.Minute), Payload: payload(map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": "head-1"})})
	// The last bounce has no later implement claim, so the user-requested-changes
	// marker stays raised.
	appendEvent(core.Event{Kind: "pipeline.bounced", At: now.Add(-39 * time.Minute), Payload: payload(map[string]any{"source": store.UserRequestChangesSource})})
	return taskID
}

// requireSameJSON compares two values by their JSON encoding, so a nil and an
// empty slice of the same fold stay distinguishable.
func requireSameJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	requireOK(t, err)
	wantJSON, err := json.Marshal(want)
	requireOK(t, err)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s = %s, want %s", label, gotJSON, wantJSON)
	}
}
