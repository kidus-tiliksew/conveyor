package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runActivityMarkerPredicates proves the activity-marker read's selection,
// claim reduction, and payload projection change no marker and no review
// lease horizon (component-web-task-surfaces invariant 19), and that rows
// the folds cannot act on, and repeated claims of one order, do not grow the
// rows the read transfers (component-persistence, Database read accounting).
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
	closed := seedMarkerHistory(t, ctx, st, ws, now, "closed", 3)
	if _, err := For(st).CancelTask(ctx, core.Intervention{TaskID: closed, Action: core.InterventionCancel, ReasonCode: "marker-predicates-closed"}); err != nil {
		t.Fatalf("cancel the closed marker task: %v", err)
	}
	page := []string{plain, excluded, shapes, backfill, shortClaims, longClaims, closed}

	read, err := st.ListActivityMarkersForTasks(ctx, page)
	requireOK(t, err)
	if len(read) != len(page) {
		t.Fatalf("ListActivityMarkersForTasks(page) = %d markers, want %d", len(read), len(page))
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
	// transfer nothing more than the history without them.
	if plainRows, excludedRows := rows(plain), rows(excluded); plainRows == 0 || excludedRows > plainRows {
		t.Fatalf("marker read rows = %d with %d excluded rows per kind, want at most the %d rows without them", excludedRows, extra, plainRows)
	}
	// A review order's older claims with past leases transfer as one claim.
	if shortRows, longRows := rows(shortClaims), rows(longClaims); shortRows == 0 || longRows > shortRows {
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
