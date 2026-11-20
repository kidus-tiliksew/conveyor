package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunActivityMarkerBoundsConformance proves the activity-marker read folds a
// bounded window of a task's marker events into the markers the task's full
// event history produces, and that the rows it transfers do not grow with that
// history (component-task-lifecycle, Compact event reads;
// component-persistence, Metrics and database read accounting).
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
	// Renewals of a task's other jobs reach no review fold and no row.
	t.Run("other job renewals", func(t *testing.T) { runActivityMarkerOtherJobRenewals(t, x, ctx, now) })

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

// runActivityMarkerOtherJobRenewals proves a task in review reads the renewals
// of its open review orders only, however long its other claims were held
// (component-web-task-surfaces invariant 19). Renewals are the one marker kind
// whose count follows how long work ran: a verify claim held for days leaves
// thousands on a task, and a review of that task must not pay for them.
//
// Two tasks hold the same open review order with a lapsed claim and one
// future renewal. Each also has an implement job and a verify job whose
// renewals name leases earlier than the review order's: per round, one
// renewal of each job in the payload the stores write and one verify
// renewal with a further key, a shape a read does not reduce and must keep
// whole when it selects it. The short task holds 4 rounds and the long one
// 1500. Two more renewals name the review order in their payloads, one on
// the verify job and one on no job. No renewal of those jobs may move the
// review lease horizon or the review diagnostic, and the long task's read
// may not transfer more rows than the short task's: a read that selected the
// renewals of every job would transfer one row per round.
func runActivityMarkerOtherJobRenewals(t *testing.T, x Fixture, ctx context.Context, now time.Time) {
	st, ws := x.Backend, x.Workspace
	const renewals = 1500
	reviewLease := now.Add(40 * time.Minute)
	seed := func(suffix string, others int) string {
		p := newPredicateSeed(t, ctx, st, ws, now, "Marker other job renewals "+suffix)
		implement := p.order("implement-1", core.StageImplement, false)
		verify := p.order("verify-1", core.StageVerify, false)
		review := p.order("review-1-seat-1", core.StageReview, false)
		p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-4*time.Hour)))
		p.append(verify.JobID, "work_order.claimed", claimPayload(verify, now.Add(-4*time.Hour)))
		p.append(review.JobID, "work_order.claimed", claimPayload(review, now.Add(-2*time.Hour)))
		p.append(review.JobID, "work_order.lease_renewed", map[string]any{"attempt_id": "attempt-review", "lease_expires_at": reviewLease})
		for i := range others {
			// Leases after the read and before the review order's: each
			// would become the horizon if the read took it for the order's.
			lease := now.Add(time.Minute + time.Duration(i)*time.Second)
			p.append(implement.JobID, "work_order.lease_renewed", map[string]any{"attempt_id": "attempt-implement", "lease_expires_at": lease})
			p.append(verify.JobID, "work_order.lease_renewed", map[string]any{"attempt_id": "attempt-verify", "lease_expires_at": lease})
			p.append(verify.JobID, "work_order.lease_renewed", map[string]any{"attempt_id": "attempt-verify", "lease_expires_at": lease, "progress": "running"})
		}
		// A renewal of another job whose payload names the review order, and
		// one of no job: neither belongs to it.
		p.append(verify.JobID, "work_order.lease_renewed", map[string]any{"id": review.ID, "lease_expires_at": now.Add(30 * time.Second)})
		p.append("", "work_order.lease_renewed", map[string]any{"review_work_order_id": review.ID, "lease_expires_at": now.Add(20 * time.Second)})
		return p.taskID
	}
	short, long := seed("short", 4), seed("long", renewals)

	for _, taskID := range []string{short, long} {
		inputs, err := st.ReadActivityInputs(ctx, []string{taskID}, now)
		requireOK(t, err)
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		if want := store.ActivityReviewLeaseHorizon(inputs.Orders, events, now); !inputs.ValidUntil.Equal(want) || !want.Equal(reviewLease) {
			t.Fatalf("task %s review lease horizon = %s, the Go fold reads %s, want the review order's renewal %s", taskID, inputs.ValidUntil, want, reviewLease)
		}
		if len(inputs.Markers) != 1 {
			t.Fatalf("task %s markers = %+v, want one", taskID, inputs.Markers)
		}
		want := markerReference(t, ctx, st, taskID, now)
		got := inputs.Markers[0]
		requireSameJSON(t, "marker "+taskID+" review diagnostics", utcReviewDiagnostics(got.ReviewDiagnostics), utcReviewDiagnostics(want.ReviewDiagnostics))
		requireSameJSON(t, "marker "+taskID+" review recovery", got.ReviewRecovery, want.ReviewRecovery)
		requireSameJSON(t, "marker "+taskID+" interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
		requireSameJSON(t, "marker "+taskID+" forge failure", got.ForgeFailure, want.ForgeFailure)
		requireSameJSON(t, "marker "+taskID+" stalled", got.Stalled, want.Stalled)
		if got.UserChangesRequested != want.UserChangesRequested {
			t.Fatalf("marker %s user changes requested = %t, want %t", taskID, got.UserChangesRequested, want.UserChangesRequested)
		}
		// The review order's lease is still running, so the order is not an
		// expired claim.
		if len(got.ReviewDiagnostics) != 0 {
			t.Fatalf("task %s review diagnostics = %+v, want none while the review lease runs", taskID, got.ReviewDiagnostics)
		}
	}
	// A minute and a half later every lease of the other jobs is past and
	// the review order's still runs; an instant after the review lease, the
	// order is an expired claim with that lease.
	for _, at := range []time.Time{now.Add(90 * time.Second), reviewLease.Add(time.Second)} {
		inputs, err := st.ReadActivityInputs(ctx, []string{long}, at)
		requireOK(t, err)
		events, err := st.ListEvents(ctx, long)
		requireOK(t, err)
		if want := store.ActivityReviewLeaseHorizon(inputs.Orders, events, at); !inputs.ValidUntil.Equal(want) {
			t.Fatalf("at %s the review lease horizon = %s, want %s", at, inputs.ValidUntil, want)
		}
		want := store.ReviewVerdictDiagnostics(inputs.Orders, events, at)
		requireSameJSON(t, "review diagnostics at "+at.Format(time.RFC3339), utcReviewDiagnostics(inputs.Markers[0].ReviewDiagnostics), utcReviewDiagnostics(want))
		if expired := at.After(reviewLease); expired != (len(want) == 1 && want[0].Status == store.ReviewExpiredWithoutVerdict && want[0].LeaseExpiresAt.Equal(reviewLease)) {
			t.Fatalf("at %s the Go fold reads diagnostics %+v, want an expired claim with the review lease exactly after it", at, want)
		}
	}

	if x.DrainReadRows == nil {
		t.Log("factory reports no read rows; the transferred-row assertion is skipped")
		return
	}
	rows := func(taskID string) uint64 {
		t.Helper()
		x.DrainReadRows()
		_, err := st.ListActivityMarkersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		return x.DrainReadRows()["ListActivityMarkersForTasks"]
	}
	if shortRows, longRows := rows(short), rows(long); shortRows == 0 || longRows == 0 || longRows > shortRows {
		t.Fatalf("the marker read transferred %d rows with %d rounds of renewals on the other jobs, want at most the %d rows of 4 rounds", longRows, renewals, shortRows)
	}
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

// runActivityMarkerPredicates proves the activity-marker read's selection,
// claim reduction, and payload projection change no marker and no review
// lease horizon (component-web-task-surfaces invariant 19), and that rows
// the folds cannot act on, and repeated claims of one order, do not grow the
// rows the read transfers (component-persistence, Metrics and database read
// accounting).
//
// Every task is read in one mixed page, with and without review orders, and
// with a closed task, and each marker is compared with the folds over the
// task's complete event list.
func runActivityMarkerPredicates(t *testing.T, x Fixture, ctx context.Context, now time.Time) {
	st, ws := x.Backend, x.Workspace
	const extra = 120
	plain := seedPredicateHistory(t, ctx, st, ws, now, "plain", 0)
	excluded := seedPredicateHistory(t, ctx, st, ws, now, "excluded", extra)
	shapes := seedPredicateShapes(t, ctx, st, ws, now)
	backfill := seedPredicateBackfill(t, ctx, st, ws, now)
	shortClaims := seedRepeatedReviewClaims(t, ctx, st, ws, now, 3)
	longClaims := seedRepeatedReviewClaims(t, ctx, st, ws, now, extra)
	// Each key shape ends its own task, so no later event masks it: a key
	// that Go folds to "source" through a non-ASCII rune, and a key stored
	// twice, whose last value both backends and Go keep.
	nonASCII := seedPredicateLastBounce(t, ctx, st, ws, now, "non-ASCII key", "{\"\u017fource\":\"user-request-changes\"}")
	duplicateRaised := seedPredicateLastBounce(t, ctx, st, ws, now, "duplicate key raised", `{"source":"operator","source":"user-request-changes"}`)
	duplicateCleared := seedPredicateLastBounce(t, ctx, st, ws, now, "duplicate key cleared", `{"source":"user-request-changes","source":"operator"}`)
	caseVariant := seedPredicateLastBounce(t, ctx, st, ws, now, "case-variant key", `{"Source":"user-request-changes"}`)
	closed := seedMarkerHistory(t, ctx, st, ws, now, "closed", 3)
	if _, err := For(st).CancelTask(ctx, core.Intervention{TaskID: closed, Action: core.InterventionCancel, ReasonCode: "marker-predicates-closed"}); err != nil {
		t.Fatalf("cancel the closed marker task: %v", err)
	}
	page := []string{plain, excluded, shapes, backfill, shortClaims, longClaims, nonASCII, duplicateRaised, duplicateCleared, caseVariant, closed}

	read, err := st.ListActivityMarkersForTasks(ctx, page)
	requireOK(t, err)
	if len(read) != len(page) {
		t.Fatalf("ListActivityMarkersForTasks(page) = %d markers, want %d", len(read), len(page))
	}
	// Each requested task must come back exactly once.
	requested := make(map[string]bool, len(page))
	for _, taskID := range page {
		requested[taskID] = true
	}
	for _, got := range read {
		if !requested[got.TaskID] {
			t.Fatalf("mixed page returned task %s, which was not requested or came back twice", got.TaskID)
		}
		delete(requested, got.TaskID)
	}
	for _, got := range read {
		want := markerReference(t, ctx, st, got.TaskID, now)
		label := "mixed page marker " + got.TaskID
		requireSameJSON(t, label+" forge failure", got.ForgeFailure, want.ForgeFailure)
		requireSameJSON(t, label+" review diagnostics", utcReviewDiagnostics(got.ReviewDiagnostics), utcReviewDiagnostics(want.ReviewDiagnostics))
		requireSameJSON(t, label+" review recovery", got.ReviewRecovery, want.ReviewRecovery)
		requireSameJSON(t, label+" interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
		requireSameJSON(t, label+" stalled", got.Stalled, want.Stalled)
		if got.UserChangesRequested != want.UserChangesRequested {
			t.Fatalf("%s user changes requested = %t, want %t", label, got.UserChangesRequested, want.UserChangesRequested)
		}
	}
	// The cases must exercise the folds, not compare two empty markers.
	byTask := map[string]store.ActivityMarker{}
	for _, marker := range read {
		byTask[marker.TaskID] = marker
	}
	if !byTask[plain].UserChangesRequested || byTask[plain].ForgeFailure == nil {
		t.Fatalf("predicate history marker = %+v, want a raised change request and a forge failure", byTask[plain])
	}
	if byTask[backfill].UserChangesRequested {
		t.Fatal("the implement claim behind an undecodable newest claim did not clear the change request")
	}
	if !byTask[caseVariant].UserChangesRequested {
		t.Fatal("the case-variant Source bounce did not raise the change request")
	}
	if !byTask[nonASCII].UserChangesRequested || !byTask[duplicateRaised].UserChangesRequested || byTask[duplicateCleared].UserChangesRequested {
		t.Fatalf("key-shape markers non-ASCII=%t duplicate raised=%t duplicate cleared=%t, want true, true, false",
			byTask[nonASCII].UserChangesRequested, byTask[duplicateRaised].UserChangesRequested, byTask[duplicateCleared].UserChangesRequested)
	}
	if len(byTask[longClaims].ReviewDiagnostics) != 1 {
		t.Fatalf("repeated review claims diagnostics = %+v, want one expired claim", byTask[longClaims].ReviewDiagnostics)
	}

	// The page's review lease horizon is the earliest one the Go fold reads
	// from any page task's complete history.
	inputs, err := st.ReadActivityInputs(ctx, page, now)
	requireOK(t, err)
	var wantHorizon time.Time
	for _, taskID := range page {
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		if lease := store.ActivityReviewLeaseHorizon(orders, events, now); !lease.IsZero() && (wantHorizon.IsZero() || lease.Before(wantHorizon)) {
			wantHorizon = lease
		}
	}
	if wantHorizon.IsZero() {
		t.Fatal("the mixed page has no review lease after now; the horizon comparison would compare two zero times")
	}
	if !inputs.ValidUntil.Equal(wantHorizon) {
		t.Fatalf("mixed page review lease horizon = %s, want %s", inputs.ValidUntil, wantHorizon)
	}

	if x.DrainReadRows == nil {
		t.Log("factory reports no read rows; the transferred-row assertions are skipped")
		return
	}
	rows := func(taskID string) uint64 {
		t.Helper()
		x.DrainReadRows()
		_, err := st.ListActivityMarkersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		return x.DrainReadRows()["ListActivityMarkersForTasks"]
	}
	// Rows no fold can act on, and repeated claims of one implement order,
	// transfer nothing more than the history without them. A backend that
	// leaves the bounce source and submission tests to Go transfers the
	// bounces of other sources and the updates that are not submissions,
	// three rows per round of the excluded history, and still no claim: the
	// two claims per round come before the task's last user change request.
	allowed := uint64(0)
	if x.MarkerReadLeavesRowTestsToGo {
		allowed = 3 * extra
	}
	if plainRows, excludedRows := rows(plain), rows(excluded); plainRows == 0 || excludedRows == 0 || excludedRows > plainRows+allowed {
		t.Fatalf("marker read rows = %d with %d excluded rows per kind, want at most the %d rows without them plus %d", excludedRows, extra, plainRows, allowed)
	}
	// A review order's older claims with past leases transfer as one claim.
	if shortRows, longRows := rows(shortClaims), rows(longClaims); shortRows == 0 || longRows == 0 || longRows > shortRows {
		t.Fatalf("marker read rows = %d for %d review claims, want at most the %d rows of 3 claims", longRows, extra, shortRows)
	}
}

// utcReviewDiagnostics compares diagnostics by instant: a backend may read
// the lease back in another zone.
func utcReviewDiagnostics(in []store.ReviewVerdictDiagnostic) []store.ReviewVerdictDiagnostic {
	out := make([]store.ReviewVerdictDiagnostic, len(in))
	copy(out, in)
	for index := range out {
		out[index].ClaimedAt = out[index].ClaimedAt.UTC()
		out[index].LeaseExpiresAt = out[index].LeaseExpiresAt.UTC()
	}
	return out
}

type predicateSeed struct {
	t      *testing.T
	ctx    context.Context
	st     store.Backend
	taskID string
	at     time.Time
}

func newPredicateSeed(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, title string) *predicateSeed {
	t.Helper()
	taskID := core.NewTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + taskID, Title: title, Body: "marker predicates",
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now.Add(-6 * time.Hour),
	}))
	return &predicateSeed{t: t, ctx: ctx, st: st, taskID: taskID, at: now.Add(-5 * time.Hour)}
}

// order creates a queued work order; a review order the case retires is
// timed out through the legal queue edge.
func (p *predicateSeed) order(suffix string, stage core.Stage, retire bool) core.WorkOrder {
	p.t.Helper()
	id := p.taskID + "-" + suffix
	requireOK(p.t, p.st.CreateJob(p.ctx, core.Job{ID: id, TaskID: p.taskID, Stage: stage, State: core.JobPending}))
	order := core.WorkOrder{ID: id, TaskID: p.taskID, JobID: id, Stage: stage, QueueEnteredAt: p.at, QueueDeadline: p.at.Add(48 * time.Hour)}
	requireOK(p.t, CreateWorkOrder(p.ctx, p.st, order))
	if retire {
		order.State = core.WorkOrderTimedOut
		requireOK(p.t, UpdateWorkOrder(p.ctx, p.st, order, core.WorkOrderCmdTimeout))
	}
	return order
}

// append stores one event a second after the previous one.
func (p *predicateSeed) append(jobID, kind string, payload any) {
	p.t.Helper()
	p.at = p.at.Add(time.Second)
	raw, ok := payload.(json.RawMessage)
	if !ok {
		encoded, err := json.Marshal(payload)
		requireOK(p.t, err)
		raw = encoded
	}
	requireOK(p.t, p.st.AppendEvent(p.ctx, core.Event{TaskID: p.taskID, JobID: jobID, Kind: kind, At: p.at.UTC(), Payload: raw}))
}

// claimPayload is the claim the stores write: the whole work order.
func claimPayload(order core.WorkOrder, lease time.Time) json.RawMessage {
	order.State = core.WorkOrderClaimed
	order.LeaseExpiresAt = lease
	return store.WorkOrderEventPayload(order)
}

// seedPredicateHistory stores a task with an implement order and a retired
// review order. Every fold input is seeded once; extra adds, per excluded
// shape, rows no fold can act on: updates of a review order, implement
// updates that are not submissions, bounces from other sources, claims of the
// retired review order, and repeated claims of the implement order.
func seedPredicateHistory(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, suffix string, extra int) string {
	t.Helper()
	p := newPredicateSeed(t, ctx, st, workspace, now, "Marker predicates "+suffix)
	implement := p.order("implement-1", core.StageImplement, false)
	review := p.order("review-1", core.StageReview, true)
	p.append("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource})
	for i := range extra {
		p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-4*time.Hour)))
		p.append("", "work_order.updated", map[string]any{"id": review.ID, "stage": "review", "state": "submitted", "head_sha": fmt.Sprintf("review-head-%d", i)})
		p.append("", "work_order.updated", map[string]any{"id": implement.ID, "stage": "implement", "state": "claimed", "head_sha": fmt.Sprintf("claimed-head-%d", i)})
		p.append("", "pipeline.bounced", map[string]any{"source": "operator"})
		p.append(review.JobID, "work_order.claimed", claimPayload(review, now.Add(-4*time.Hour)))
	}
	p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-3*time.Hour)))
	p.append("", "work_order.updated", map[string]any{"id": implement.ID, "stage": "implement", "state": "submitted", "head_sha": "head-1"})
	p.append(review.JobID, "review.completed", map[string]any{"review_work_order_id": review.ID, "review_round": 1, "verdict": "changes_requested"})
	p.append("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict"})
	p.append("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource})
	return p.taskID
}

// seedPredicateShapes stores payloads whose keys the database cannot match
// the way Go does, so the read keeps them whole: a case-variant key, a
// non-ASCII key that Go folds to an ASCII field name, a key only nested in
// another object, escaped strings, and a value of the wrong type.
func seedPredicateShapes(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time) string {
	t.Helper()
	p := newPredicateSeed(t, ctx, st, workspace, now, "Marker payload shapes")
	implement := p.order("implement-1", core.StageImplement, false)
	raw := func(text string) json.RawMessage { return json.RawMessage(text) }
	p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-4*time.Hour)))
	p.append("", "pipeline.bounced", raw(`{"Source":"user-request-changes"}`))
	p.append(implement.JobID, "work_order.claimed", raw(`{"STAGE":"implement","state":"claimed"}`))
	p.append("", "pipeline.bounced", raw(`{"meta":{"source":"user-request-changes"}}`))
	p.append("", "pipeline.bounced", raw(`{"ſource":"user-request-changes"}`))
	p.append("", "work_order.updated", raw(`{"id":"`+implement.ID+`","Stage":"implement","state":"submitted","head_sha":"head-case"}`))
	p.append("", "work_order.updated", raw(`{"id":"`+implement.ID+`","stage":"implement","state":"submitted","head_sha":"head-escaped"}`))
	p.append("", "merge.failed", raw(`{"forge_error_category":"merge_conflict","error":"say \"no\"","HEAD_SHA":"head-case"}`))
	p.append("", "github_issue.publication_retry", raw(`{"automatic":"true"}`))
	p.append("", "github_issue.publication_failed", raw(`{"forge_error_category":"issue_failed","last_error":"issue failed"}`))
	p.append("", "review.publication_failed", raw(`{"review_work_order_id":"review-x","forge_error_category":5,"last_error":"wrong type"}`))
	p.append("", "pipeline.bounced", raw(`{"source":"user-request-changes","Source":"operator"}`))
	return p.taskID
}

// seedPredicateLastBounce stores an implement claim followed by one bounce
// whose payload is written exactly as given, so the change-request marker
// depends only on how that payload's key is read.
func seedPredicateLastBounce(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, title, bounce string) string {
	t.Helper()
	p := newPredicateSeed(t, ctx, st, workspace, now, "Marker "+title)
	implement := p.order("implement-1", core.StageImplement, false)
	p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-4*time.Hour)))
	p.append("", "pipeline.bounced", json.RawMessage(bounce))
	return p.taskID
}

// seedPredicateBackfill stores an implement claim that clears a change
// request, followed by a newer claim of the same order whose payload names
// the implement stage but does not decode as a work order. The reduction
// leaves the older claim out; the read must take it back.
func seedPredicateBackfill(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time) string {
	t.Helper()
	p := newPredicateSeed(t, ctx, st, workspace, now, "Marker claim backfill")
	implement := p.order("implement-1", core.StageImplement, false)
	p.append("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource})
	p.append(implement.JobID, "work_order.claimed", claimPayload(implement, now.Add(-4*time.Hour)))
	p.append(implement.JobID, "work_order.claimed", json.RawMessage(`{"id":"`+implement.ID+`","stage":"implement","attempt_id":5}`))
	return p.taskID
}

// seedRepeatedReviewClaims stores an open review order claimed claims times,
// each claim's lease in the past, and the newest claim's lease still in the
// past, so the order is an expired claim without a verdict. A second open
// order holds a future lease so the page has a review lease horizon.
func seedRepeatedReviewClaims(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, claims int) string {
	t.Helper()
	p := newPredicateSeed(t, ctx, st, workspace, now, fmt.Sprintf("Marker repeated review claims %d", claims))
	expired := p.order("review-1-seat-1", core.StageReview, false)
	open := p.order("review-1-seat-2", core.StageReview, false)
	for i := range claims {
		p.append(expired.JobID, "work_order.claimed", claimPayload(expired, now.Add(-4*time.Hour+time.Duration(i)*time.Second)))
	}
	p.append(expired.JobID, "work_order.claimed", claimPayload(expired, now.Add(-time.Hour)))
	p.append(open.JobID, "work_order.claimed", claimPayload(open, now.Add(45*time.Minute)))
	return p.taskID
}

// RunActivityMarkerReviewRoundsConformance proves that every marker read
// gives the review recovery folds the review-round kinds they decode,
// review.round_completed, review.round_retried, and review.round_recovered,
// so each marker equals the folds over the task's complete event list
// (component-web-task-surfaces invariant 19; req-bounded-automatic-recovery
// AC-4.2). Store commands reach every history:
//
//   - A fail-fast round settles while a sibling seat is timed out. Settlement
//     cancels that seat, so no settled round keeps a timed-out seat and no
//     marker reports a recovery for it. This is the control.
//   - An automatic retry creates the latest round, and that round's seats
//     time out again. The retry recorded the round's one automatic act, so
//     the recovery marker reports automatic_retry_exhausted.
//   - An automatic recovery restarts an interrupted seat in place, and the
//     seat is interrupted again. The interrupted-recovery marker reports
//     automatic_retry_exhausted for the same reason.
//
// The page also holds a task with a live review claim, so the review lease
// horizon is compared on a read that selects leases.
func RunActivityMarkerReviewRoundsConformance(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.SystemActor())
	restore := setAutomaticRecoveryBound(t, st, ctx, 1)
	defer restore()

	settled := seedSettledRoundWithTimedOutSibling(t, x)
	retried := seedAutomaticRetryRoundTimedOut(t, st, ctx, ws)
	recovered := seedAutomaticRecoveryInterruptedAgain(t, st, ctx, ws)
	live, _ := seedReviewRoundWithVerdict(t, st, ctx, ws)
	liveOrders, err := st.ListTaskWorkOrders(ctx, live.ID)
	requireOK(t, err)
	for _, order := range liveOrders {
		if order.Stage == core.StageReview && order.State == core.WorkOrderQueued {
			_, err = ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "live-" + order.ID, ClientToken: "live", Lease: time.Hour, ExecutionTimeout: time.Hour})
			requireOK(t, err)
		}
	}
	page := []string{settled, retried, recovered, live.ID}
	// The folds compare leases with this instant. Every expired lease above
	// is already past it, and the live claim's lease is an hour after it.
	now := time.Now().UTC().Truncate(time.Microsecond)

	// The histories must reach the folds, so the comparisons below cannot
	// pass on two empty markers.
	if want := markerReference(t, ctx, st, settled, now); want.ReviewRecovery != nil || want.InterruptedReviewRecovery != nil {
		t.Fatalf("settled round reference recovery=%+v interrupted=%+v, want none", want.ReviewRecovery, want.InterruptedReviewRecovery)
	}
	if want := markerReference(t, ctx, st, retried, now).ReviewRecovery; want == nil || want.PriorRound != 2 || len(want.TimedOutOrders) != 2 || !want.AutomaticRetryExhausted {
		t.Fatalf("retried round reference recovery=%+v, want round 2 with two timed-out seats and the automatic act spent", want)
	}
	if want := markerReference(t, ctx, st, recovered, now).InterruptedReviewRecovery; want == nil || want.ReviewRound != 1 || !want.AutomaticRetryExhausted {
		t.Fatalf("recovered round reference interrupted recovery=%+v, want round 1 with the automatic act spent", want)
	}

	compare := func(t *testing.T, label string, got store.ActivityMarker) {
		t.Helper()
		want := markerReference(t, ctx, st, got.TaskID, now)
		label += " " + got.TaskID
		requireSameJSON(t, label+" forge failure", got.ForgeFailure, want.ForgeFailure)
		requireSameJSON(t, label+" review diagnostics", utcReviewDiagnostics(got.ReviewDiagnostics), utcReviewDiagnostics(want.ReviewDiagnostics))
		requireSameJSON(t, label+" review recovery", got.ReviewRecovery, want.ReviewRecovery)
		requireSameJSON(t, label+" interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
		requireSameJSON(t, label+" stalled", got.Stalled, want.Stalled)
		if got.UserChangesRequested != want.UserChangesRequested {
			t.Fatalf("%s user changes requested = %t, want %t", label, got.UserChangesRequested, want.UserChangesRequested)
		}
	}
	// Each history alone, in its own subtest so each reports its own
	// result. Only the live task has a live review claim, so the reads
	// without lease selection run too.
	for _, single := range []struct{ name, taskID string }{
		{"SettledRoundControl", settled}, {"AutomaticRetryTimedOutAgain", retried},
		{"AutomaticRecoveryInterruptedAgain", recovered}, {"LiveReviewClaim", live.ID},
	} {
		t.Run(single.name, func(t *testing.T) {
			read, err := st.ListActivityMarkersForTasks(ctx, []string{single.taskID})
			requireOK(t, err)
			if len(read) != 1 || read[0].TaskID != single.taskID {
				t.Fatalf("ListActivityMarkersForTasks(%s) = %+v, want one marker", single.taskID, read)
			}
			compare(t, "single-task marker", read[0])
		})
	}
	t.Run("Page", func(t *testing.T) {
		read, err := st.ListActivityMarkersForTasks(ctx, page)
		requireOK(t, err)
		if len(read) != len(page) {
			t.Fatalf("ListActivityMarkersForTasks(page) = %d markers, want %d", len(read), len(page))
		}
		for _, got := range read {
			compare(t, "page marker", got)
		}
		all, err := st.ListActivityMarkers(ctx)
		requireOK(t, err)
		found := 0
		for _, got := range all {
			if slices.Contains(page, got.TaskID) {
				found++
				compare(t, "workspace marker", got)
			}
		}
		if found != len(page) {
			t.Fatalf("ListActivityMarkers returned %d of the %d review-round tasks", found, len(page))
		}
		inputs, err := st.ReadActivityInputs(ctx, page, now)
		requireOK(t, err)
		if len(inputs.Markers) != len(page) {
			t.Fatalf("ReadActivityInputs(page) = %d markers, want %d", len(inputs.Markers), len(page))
		}
		for _, got := range inputs.Markers {
			compare(t, "activity input marker", got)
		}
		var wantHorizon time.Time
		for _, taskID := range page {
			orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
			requireOK(t, err)
			if lease := store.ActivityReviewLeaseHorizon(orders, listEventsOK(t, st, ctx, taskID), now); !lease.IsZero() && (wantHorizon.IsZero() || lease.Before(wantHorizon)) {
				wantHorizon = lease
			}
		}
		if wantHorizon.IsZero() {
			t.Fatal("the review-round page has no review lease after now; the horizon comparison would compare two zero times")
		}
		if !inputs.ValidUntil.Equal(wantHorizon) {
			t.Fatalf("review-round page lease horizon = %s, want %s", inputs.ValidUntil, wantHorizon)
		}
	})
}

// seedSettledRoundWithTimedOutSibling opens a two-seat fail-fast round,
// times out seat 2 while it holds a claim, and settles the round with a
// changes-requested verdict from seat 1. Settlement cancels the timed-out
// seat (task body scenario A cannot be reached through store commands).
func seedSettledRoundWithTimedOutSibling(t *testing.T, x Fixture) string {
	t.Helper()
	task, orders := ffRound(t, x, 2, true)
	sibling := orders[1]
	ffClaim(t, x, sibling)
	timedOut := ffOrder(t, x, sibling.ID)
	timedOut.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, timedOut, core.WorkOrderCmdTimeout))
	if got := ffOrder(t, x, sibling.ID); got.State != core.WorkOrderTimedOut {
		t.Fatalf("sibling seat state = %s, want timed_out before settlement", got.State)
	}
	ffVerdict(t, x, orders[0], "changes_requested")
	if _, count := ffRoundCompleted(t, x, task.ID); count != 1 {
		t.Fatalf("fail-fast round settled %d times, want once", count)
	}
	if got := ffOrder(t, x, sibling.ID); got.State != core.WorkOrderCancelled {
		t.Fatalf("timed-out sibling seat state after settlement = %s, want cancelled", got.State)
	}
	return task.ID
}

// seedAutomaticRetryRoundTimedOut times out seat 2 of round 1, retries the
// round automatically, and times out both seats of the new round 2.
func seedAutomaticRetryRoundTimedOut(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	task, orders := seedReviewRoundWithVerdict(t, st, ctx, workspace)
	timeOutReviewSeat(t, st, ctx, orders[1].ID)
	jobs, next := reviewRound(task.ID, 2)
	request := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 1), PriorRound: 1, Reason: store.AutomaticReviewReason, PRHead: "fixture-head", Automatic: true}
	result, err := RetryReviewRound(ctx, st, request, jobs, next)
	if err != nil || result.NewRound != 2 || len(result.WorkOrders) != 2 {
		t.Fatalf("automatic retry result=%+v err=%v", result, err)
	}
	if retried := automaticRoundEvents(t, st, ctx, task.ID, "review.round_retried"); len(retried) != 1 {
		t.Fatalf("automatic round_retried events=%+v, want one", retried)
	}
	for _, order := range result.WorkOrders {
		timeOutReviewSeat(t, st, ctx, order.ID)
	}
	return task.ID
}

// seedAutomaticRecoveryInterruptedAgain loses seat 2's lease, recovers the
// round automatically in place, and loses the seat's lease again.
func seedAutomaticRecoveryInterruptedAgain(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	task, orders := seedReviewRoundWithVerdict(t, st, ctx, workspace)
	seat := orders[1].ID
	expireReviewSeat(t, st, ctx, seat, "marker-first")
	request := store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 1), Round: 1, Automatic: true}
	if _, err := RecoverInterruptedReviewRound(ctx, st, request, time.Hour); err != nil {
		t.Fatalf("automatic recovery err=%v", err)
	}
	if recovered := automaticRoundEvents(t, st, ctx, task.ID, "review.round_recovered"); len(recovered) != 1 {
		t.Fatalf("automatic round_recovered events=%+v, want one", recovered)
	}
	expireReviewSeat(t, st, ctx, seat, "marker-second")
	return task.ID
}

// timeOutReviewSeat claims a queued review seat and times it out through the
// timeout command.
func timeOutReviewSeat(t *testing.T, st store.Store, ctx context.Context, id string) {
	t.Helper()
	claimed, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "timeout-" + id, ClientToken: "timeout-" + id, Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	claimed.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdTimeout))
}
