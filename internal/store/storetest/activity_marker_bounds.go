package storetest

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

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

// MarkerOtherJobClaimsTask is one task of the other-job claims bound
// (SeedMarkerOtherJobClaims): its open review order and the lease the review
// lease horizon must read.
type MarkerOtherJobClaimsTask struct {
	TaskID      string
	Review      core.WorkOrder
	ReviewLease time.Time
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
