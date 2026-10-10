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
