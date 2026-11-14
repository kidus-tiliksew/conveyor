package store

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestMemoryGitHubLifecycleOnlyEmitsActivityForOutcomesAndRealRetries(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "test")
	st := NewMemory()
	task := core.Task{ID: "github-lifecycle-events", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: 1}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("lifecycle ok=%t err=%v", ok, err)
	}
	lifecycle.State = core.GitHubPublicationRetrying
	lifecycle.Attempts = 1
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	lifecycle.CreateState = core.GitHubCreateReconciling
	lifecycle.CreateAttempts = 1
	lifecycle.LastError = "   "
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_retry"); countErr != nil || count != 0 {
		t.Fatalf("state-only retry events=%d err=%v", count, countErr)
	}
	lifecycle.LastError = "GitHub returned 503"
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_retry"); countErr != nil || count != 1 {
		t.Fatalf("real retry events=%d err=%v", count, countErr)
	}
	lifecycle.State = core.GitHubPublicationFailed
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_failed"); countErr != nil || count != 1 {
		t.Fatalf("failed events=%d err=%v", count, countErr)
	}
}

func TestLatestForgeFailureTracksOnlyUnresolvedOperatorEvidence(t *testing.T) {
	at := time.Now().UTC()
	events := []core.Event{
		{Kind: "github_issue.publication_failed", At: at, Payload: core.JSONPayload(map[string]any{"forge_error_category": "forge_request", "last_error": "request timed out"})},
		{Kind: "github_issue.publication_published", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{})},
		{Kind: "review.publication_failed", JobID: "review-1", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "forge_error_category": "forge_permission", "last_error": "comment denied"})},
		{Kind: "merge.failed", At: at.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{"forge_error_category": "forge_rate_limited", "error": "rate limit exceeded"})},
	}
	failure := LatestForgeFailure(events)
	if failure == nil || failure.Category != "forge_rate_limited" || failure.Detail != "rate limit exceeded" || failure.Surface != "GitHub merge" {
		t.Fatalf("failure=%+v", failure)
	}
	events = append(events, core.Event{Kind: "merge.confirmed", At: at.Add(4 * time.Second), Payload: core.JSONPayload(map[string]any{})})
	failure = LatestForgeFailure(events)
	if failure == nil || failure.Category != "forge_permission" || failure.Detail != "comment denied" {
		t.Fatalf("failure after merge resolution=%+v", failure)
	}
	events = append(events, core.Event{Kind: "review.publication_published", JobID: "review-1", At: at.Add(5 * time.Second), Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1"})})
	if failure = LatestForgeFailure(events); failure != nil {
		t.Fatalf("resolved failures remain actionable: %+v", failure)
	}
}

func TestLatestForgeFailureSurfacesConflictDispatchAndRecoveryBlockers(t *testing.T) {
	at := time.Now().UTC()
	events := []core.Event{{
		Kind: "merge.conflict_recovery_blocked", At: at,
		Payload: core.JSONPayload(map[string]any{"error": "latest review round has interrupted seats"}),
	}}
	failure := LatestForgeFailure(events)
	if failure == nil || failure.Category != "interrupted_review_recovery" || failure.Surface != "Merge conflict recovery" {
		t.Fatalf("recovery failure=%+v", failure)
	}
	events = append(events,
		core.Event{Kind: "merge.conflict_fix_dispatched", At: at.Add(time.Second)},
		core.Event{Kind: "merge.conflict_dispatch_exhausted", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"error": "work order insert failed"})},
	)
	failure = LatestForgeFailure(events)
	if failure == nil || failure.Category != "conflict_dispatch_exhausted" || failure.Detail != "work order insert failed" || failure.Surface != "Merge conflict dispatch" {
		t.Fatalf("dispatch failure=%+v", failure)
	}
	events = append(events, core.Event{Kind: "merge.conflict_cleared", At: at.Add(3 * time.Second)})
	if failure = LatestForgeFailure(events); failure != nil {
		t.Fatalf("cleared conflict remains actionable: %+v", failure)
	}
}

// mergeTakeoverEvents is an approved head's failed merge: the implement
// order submitted approved-head, then GitHub refused the merge.
func mergeTakeoverEvents(at time.Time) []core.Event {
	return []core.Event{
		{Kind: "work_order.updated", JobID: "task-implement-1", At: at, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-1", Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: "approved-head"})},
		{Kind: "merge.requested", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": 12})},
		{Kind: "merge.failed", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed", "forge_error_category": "forge_request", "error": "Base branch was modified"})},
	}
}

func requireMergeFailure(t *testing.T, label string, failure *ForgeFailure, detail string) {
	t.Helper()
	if failure == nil || failure.Surface != "GitHub merge" || failure.Detail != detail {
		t.Fatalf("%s: failure=%+v, want the GitHub merge failure %q", label, failure, detail)
	}
}

// A merge failure stops being the operator's once a later event shows the
// factory or an agent took it over (task 261114-38bea5;
// req-operator-next-action AC-1.1; DEC-117(3); component-next-action).
func TestLatestForgeFailureClearsMergeFailureOnTakeover(t *testing.T) {
	at := time.Now().UTC()
	requireMergeFailure(t, "unanswered", LatestForgeFailure(mergeTakeoverEvents(at)), "Base branch was modified")
	later := at.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		after []core.Event
	}{
		{"conflict fix dispatched", []core.Event{
			{Kind: "merge.blocked", At: later, Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict", "approved_head": "approved-head", "new_head": "approved-head"})},
			{Kind: "merge.conflict_fix_dispatched", At: later.Add(time.Second), Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict", "approved_head": "approved-head", "work_order_id": "task-implement-2"})},
		}},
		{"replacement head submitted", []core.Event{
			{Kind: "work_order.updated", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderClaimed})},
			{Kind: "work_order.updated", At: later.Add(time.Second), Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: "replacement-head"})},
		}},
		{"merge confirmed", []core.Event{{Kind: "merge.confirmed", At: later, Payload: core.JSONPayload(map[string]any{"head_sha": "approved-head"})}}},
		{"merge reconciled", []core.Event{{Kind: "merge.reconciled", At: later, Payload: core.JSONPayload(map[string]any{"head_sha": "approved-head"})}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append(mergeTakeoverEvents(at), tc.after...)
			if failure := LatestForgeFailure(events); failure != nil {
				t.Fatalf("failure=%+v, want none after %s", failure, tc.name)
			}
			// A later failed merge is the operator's again.
			events = append(events, core.Event{Kind: "merge.failed", At: later.Add(time.Minute), Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed", "error": "second refusal"})})
			requireMergeFailure(t, "after a later failure", LatestForgeFailure(events), "second refusal")
		})
	}
}

// Events that neither take the failure over nor merge leave it with the
// operator (task 261114-38bea5; req-operator-next-action AC-1.1).
func TestLatestForgeFailureKeepsUnansweredMergeFailure(t *testing.T) {
	at := time.Now().UTC()
	later := at.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		after []core.Event
	}{
		{"merge requested", []core.Event{{Kind: "merge.requested", At: later, Payload: core.JSONPayload(map[string]any{"repository": "acme/app"})}}},
		{"conflict episode opened", []core.Event{{Kind: "merge.blocked", At: later, Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict"})}}},
		{"conflict cleared", []core.Event{{Kind: "merge.conflict_cleared", At: later, Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict"})}}},
		{"conflict dispatch failed", []core.Event{
			{Kind: "merge.blocked", At: later, Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict"})},
			{Kind: "merge.conflict_dispatch_failed", At: later.Add(time.Second), Payload: core.JSONPayload(map[string]any{"error": "insert failed"})},
		}},
		{"conflict dispatch without an order", []core.Event{{Kind: "merge.conflict_fix_dispatched", At: later, Payload: core.JSONPayload(map[string]any{"reason_code": "merge-conflict"})}}},
		{"malformed conflict dispatch", []core.Event{{Kind: "merge.conflict_fix_dispatched", At: later, Payload: []byte(`{"work_order_id":`)}}},
		{"pull request opened", []core.Event{{Kind: "pull_request.opened", At: later, Payload: core.JSONPayload(map[string]any{"number": 12, "head_sha": "replacement-head"})}}},
		{"implement order claimed", []core.Event{
			{Kind: "work_order.claimed", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderClaimed})},
			{Kind: "work_order.updated", At: later.Add(time.Second), Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderClaimed, HeadSHA: "replacement-head"})},
		}},
		{"malformed submission", []core.Event{{Kind: "work_order.updated", At: later, Payload: []byte(`{"id":"task-implement-2","stage":"implement","state":"submitted","head_sha":`)}}},
		{"submission without a head", []core.Event{{Kind: "work_order.updated", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderSubmitted})}}},
		{"review order submitted", []core.Event{{Kind: "work_order.updated", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-review-1-seat-1", Stage: core.StageReview, State: core.WorkOrderSubmitted, HeadSHA: "replacement-head"})}}},
		{"failed head submitted again", []core.Event{{Kind: "work_order.updated", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-2", Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: "approved-head"})}}},
		{"earlier submission updated again", []core.Event{{Kind: "work_order.updated", At: later, Payload: core.JSONPayload(core.WorkOrder{ID: "task-implement-1", Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: "approved-head"})}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireMergeFailure(t, tc.name, LatestForgeFailure(append(mergeTakeoverEvents(at), tc.after...)), "Base branch was modified")
		})
	}
}

// A failure that names its head, or one recorded before any submission,
// clears on a later submission of another head (task 261114-38bea5).
func TestLatestForgeFailureMatchesSubmissionToFailedHead(t *testing.T) {
	at := time.Now().UTC()
	mismatch := core.Event{Kind: "merge.failed", At: at, Payload: core.JSONPayload(map[string]any{"reason_code": "merged_head_mismatch", "expected_head_sha": "expected-head", "error": "merged at another head"})}
	submitted := func(id, head string) core.Event {
		return core.Event{Kind: "work_order.updated", At: at.Add(time.Second), Payload: core.JSONPayload(core.WorkOrder{ID: id, Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: head})}
	}
	requireMergeFailure(t, "expected head resubmitted", LatestForgeFailure([]core.Event{mismatch, submitted("implement-2", "expected-head")}), "merged at another head")
	if failure := LatestForgeFailure([]core.Event{mismatch, submitted("implement-2", "new-head")}); failure != nil {
		t.Fatalf("failure=%+v after a new head", failure)
	}
	headless := core.Event{Kind: "merge.failed", At: at, Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed", "error": "refused"})}
	if failure := LatestForgeFailure([]core.Event{headless, submitted("implement-1", "first-head")}); failure != nil {
		t.Fatalf("failure=%+v after the first submission", failure)
	}
}

// Clearing a merge failure leaves the issue, review publication, and
// conflict recovery failures (task 261114-38bea5).
func TestLatestForgeFailureMergeTakeoverKeepsIndependentFailures(t *testing.T) {
	at := time.Now().UTC()
	base := []core.Event{
		{Kind: "github_issue.publication_failed", At: at, Payload: core.JSONPayload(map[string]any{"forge_error_category": "forge_request", "last_error": "issue refused"})},
		{Kind: "review.publication_failed", JobID: "review-1", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "last_error": "comment refused"})},
		{Kind: "merge.conflict_dispatch_exhausted", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"error": "conflict budget exhausted"})},
		{Kind: "merge.failed", At: at.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{"error": "Base branch was modified"})},
	}
	for _, tc := range []struct {
		name     string
		takeover core.Event
	}{
		{"submission", core.Event{Kind: "work_order.updated", At: at.Add(4 * time.Second), Payload: core.JSONPayload(core.WorkOrder{ID: "implement-2", Stage: core.StageImplement, State: core.WorkOrderSubmitted, HeadSHA: "new-head"})}},
		{"confirmed merge", core.Event{Kind: "merge.confirmed", At: at.Add(4 * time.Second), Payload: core.JSONPayload(map[string]any{})}},
		{"reconciled merge", core.Event{Kind: "merge.reconciled", At: at.Add(4 * time.Second), Payload: core.JSONPayload(map[string]any{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append(append([]core.Event(nil), base...), tc.takeover)
			failure := LatestForgeFailure(events)
			if failure == nil || failure.Surface != "Merge conflict dispatch" || failure.Detail != "conflict budget exhausted" {
				t.Fatalf("failure=%+v, want the conflict dispatch failure", failure)
			}
			events = append(events, core.Event{Kind: "merge.conflict_cleared", At: at.Add(5 * time.Second)})
			if failure = LatestForgeFailure(events); failure == nil || failure.Surface != "GitHub review publication" {
				t.Fatalf("failure=%+v, want the review publication failure", failure)
			}
			events = append(events, core.Event{Kind: "review.publication_published", JobID: "review-1", At: at.Add(6 * time.Second), Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1"})})
			if failure = LatestForgeFailure(events); failure == nil || failure.Surface != "GitHub issue publication" {
				t.Fatalf("failure=%+v, want the issue publication failure", failure)
			}
		})
	}
}
