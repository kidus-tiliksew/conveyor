package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// lockCountingStore records the tasks whose side-effect lock was taken.
type lockCountingStore struct {
	store.Store
	mu    sync.Mutex
	locks map[string]int
}

func (s *lockCountingStore) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]int{}
	}
	s.locks[taskID]++
	s.mu.Unlock()
	return s.Store.WithTaskSideEffectLock(ctx, taskID, fn)
}

func (s *lockCountingStore) count(taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locks[taskID]
}

func mergeEventKinds(t *testing.T, st store.Store, ctx context.Context, taskID string) []string {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "merge.") {
			kinds = append(kinds, event.Kind)
		}
	}
	return kinds
}

// TestApplyPullRequestDeliveryReconcilesExternalMerge: a pull_request
// delivery reaches ReconcileObservedPullRequest for the task holding the
// branch under its side-effect lock. An unmerged delivery changes nothing; a
// merged one completes the approved task through merge.reconciled with no
// GitHub read or merge request; a delivery for another repository is ignored
// (component-submission-merge, Merge readiness from webhook deliveries;
// req-quiet-idle-runtime AC-3.1; req-delivery-and-forge AC-3.5).
func TestApplyPullRequestDeliveryReconcilesExternalMerge(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	openPullRequest(t, st, ctx, task.ID, 41, "approved-head")
	locks := &lockCountingStore{Store: st}
	d.Store = locks
	forbidMergeCalls(t, d)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		t.Fatal("a pull_request delivery read GitHub")
		return githubtrigger.PullRequest{}, nil
	}
	open := githubtrigger.PullRequest{Number: 41, State: "open", Mergeable: "UNKNOWN", HeadSHA: "approved-head", HeadRef: task.Branch}
	if err := d.ApplyPullRequestDelivery(ctx, "acme/app", open); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved {
		t.Fatalf("unmerged delivery changed the task: %s", current.State)
	}
	if kinds := mergeEventKinds(t, st, ctx, task.ID); len(kinds) != 0 {
		t.Fatalf("unmerged delivery recorded %v", kinds)
	}
	if err := d.ApplyPullRequestDelivery(ctx, "acme/other", mergedPull(task, 41, "approved-head", "landed", "github-operator")); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved {
		t.Fatalf("another repository's delivery changed the task: %s", current.State)
	}
	if err := d.ApplyPullRequestDelivery(ctx, "ACME/App", mergedPull(task, 41, "approved-head", "landed", "github-operator")); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("merged delivery task=%+v err=%v", current, err)
	}
	if kinds := mergeEventKinds(t, st, ctx, task.ID); len(kinds) != 1 || kinds[0] != "merge.reconciled" {
		t.Fatalf("merge events=%v, want one merge.reconciled", kinds)
	}
	if locks.count(task.ID) < 2 {
		t.Fatalf("task side-effect lock taken %d times", locks.count(task.ID))
	}
}

// TestApplyChecksDeliveryAutoMergesGateOffApprovedTask: a completed check
// delivery for the approved head merges a gate-off approved task once
// (merge.requested, merge.confirmed) under its side-effect lock, reads no
// task of another branch or head, and a repeat changes nothing
// (req-review-gates-evidence AC-1.1; req-quiet-idle-runtime AC-3.1).
func TestApplyChecksDeliveryAutoMergesGateOffApprovedTask(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	other := core.Task{ID: "other-task", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/other-task", State: core.TaskApproved, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, other); err != nil {
		t.Fatal(err)
	}
	locks := &lockCountingStore{Store: st}
	d.Store = locks
	var mu sync.Mutex
	reads := map[string]int{}
	merged := false
	d.ViewPullRequest = func(_ context.Context, repo, branch string) (githubtrigger.PullRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		reads[branch]++
		if merged {
			return githubtrigger.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "approved-head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return githubtrigger.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "approved-head", BaseSHA: "base"}, nil
	}
	merges := 0
	d.RequestMerge = func(context.Context, string, int) error {
		mu.Lock()
		defer mu.Unlock()
		merges++
		merged = true
		return nil
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "unrelated-head", []string{"feature/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 0 || merges != 0 {
		t.Fatalf("unrelated delivery reads=%v merges=%d", reads, merges)
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "approved-head", nil); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || merges != 1 {
		t.Fatalf("task=%+v merges=%d err=%v", current, merges, err)
	}
	if reads[other.Branch] != 0 {
		t.Fatalf("delivery read the other task's branch: %v", reads)
	}
	if kinds := mergeEventKinds(t, st, ctx, task.ID); strings.Join(kinds, ",") != "merge.requested,merge.confirmed" {
		t.Fatalf("merge events=%v", kinds)
	}
	if locks.count(task.ID) == 0 {
		t.Fatal("check delivery applied without the task side-effect lock")
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "approved-head", []string{task.Branch}); err != nil {
		t.Fatal(err)
	}
	if merges != 1 {
		t.Fatalf("repeated delivery merged again: %d", merges)
	}
}

// TestApplyChecksDeliveryLeavesGateOnTaskUnmerged: with the merge gate on, a
// check delivery reads only the merged short-circuit and never requests a
// merge.
func TestApplyChecksDeliveryLeavesGateOnTaskUnmerged(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	forbidMergeCalls(t, d)
	reads := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		reads++
		return githubtrigger.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "approved-head"}, nil
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "", []string{task.Branch}); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved || reads != 1 {
		t.Fatalf("task=%+v reads=%d err=%v", current, reads, err)
	}
	if kinds := mergeEventKinds(t, st, ctx, task.ID); len(kinds) != 0 {
		t.Fatalf("gate-on delivery recorded %v", kinds)
	}
}

// TestRetryableMergeErrorClassifies: deferrals the merge path already
// recorded are not retried; transient GitHub failures, unsettled
// mergeability, and store or configuration failures are
// (component-submission-merge, Merge readiness from webhook deliveries).
func TestRetryableMergeErrorClassifies(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pending conflict fix", fmt.Errorf("%w: work order x is queued", ErrConflictFixPending), false},
		{"ineligible", ineligibleMerge(errors.New("pull request is closed without a merge")), false},
		{"missing pull request", fmt.Errorf("no pull request: %w", githubtrigger.ErrPullRequestNotFound), false},
		{"merge refused 405", fmt.Errorf("GitHub could not merge: %w", githubtrigger.StatusError(405, errors.New("required status check is expected"))), false},
		{"permission", githubtrigger.PermissionError(errors.New("App lacks access")), false},
		{"cancelled", context.Canceled, false},
		{"GitHub 502", fmt.Errorf("could not read the pull request: %w", githubtrigger.StatusError(502, errors.New("bad gateway"))), true},
		{"rate limited", &githubtrigger.Error{Category: githubtrigger.ForgeRateLimited, Err: errors.New("rate limit")}, true},
		{"unsettled mergeability", fmt.Errorf("pull request acme/app#1 %w", ErrMergeReadinessPending), true},
		{"store failure", errors.New("database unavailable"), true},
	}
	for _, tc := range cases {
		if got := retryableMergeError(tc.err); got != tc.want {
			t.Errorf("%s: retryable=%t, want %t", tc.name, got, tc.want)
		}
	}
}

// failOnceStore fails the first merge.requested append.
type failOnceStore struct {
	store.Store
	failed atomic.Bool
}

func (s *failOnceStore) AppendEvent(ctx context.Context, event core.Event) error {
	if event.Kind == "merge.requested" && s.failed.CompareAndSwap(false, true) {
		return errors.New("database unavailable")
	}
	return s.Store.AppendEvent(ctx, event)
}

// TestApplyChecksDeliveryReturnsRetryableFailures: a transient GitHub read
// failure on one matching task is returned after the other matching task
// merges; a store failure is returned and the next application merges; a
// 405 merge refusal records merge.failed and is not returned
// (req-quiet-idle-runtime AC-3.1).
func TestApplyChecksDeliveryReturnsRetryableFailures(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "shared-head"); err != nil {
		t.Fatal(err)
	}
	second := core.Task{ID: "second-task", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/second-task", State: core.TaskApproved, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, second.ID, "shared-head"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	merged := map[string]bool{}
	failFirst := true
	d.ViewPullRequest = func(_ context.Context, _, branch string) (githubtrigger.PullRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		if branch == task.Branch && failFirst {
			failFirst = false
			return githubtrigger.PullRequest{}, githubtrigger.StatusError(502, errors.New("bad gateway"))
		}
		number := 41
		if branch == second.Branch {
			number = 42
		}
		if merged[branch] {
			return githubtrigger.PullRequest{Number: number, State: "closed", Merged: true, HeadSHA: "shared-head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return githubtrigger.PullRequest{Number: number, State: "open", Mergeable: "MERGEABLE", HeadSHA: "shared-head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(_ context.Context, _ string, number int) error {
		mu.Lock()
		defer mu.Unlock()
		if number == 41 {
			merged[task.Branch] = true
		} else {
			merged[second.Branch] = true
		}
		return nil
	}
	err := d.ApplyChecksDelivery(ctx, "acme/app", "shared-head", nil)
	if err == nil || !retryableMergeError(err) || !strings.Contains(err.Error(), task.ID) {
		t.Fatalf("transient read failure returned %v", err)
	}
	if current, _ := st.GetTask(ctx, second.ID); current.State != core.TaskMerged {
		t.Fatalf("the remaining matching task was not applied: %s", current.State)
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "shared-head", nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged {
		t.Fatalf("retry did not merge: %s", current.State)
	}

	ctx, st, task, d = approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	failing := &failOnceStore{Store: st}
	d.Store = failing
	mergedOnce := false
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		if mergedOnce {
			return githubtrigger.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "approved-head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return githubtrigger.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "approved-head", BaseSHA: "base"}, nil
	}
	merges := 0
	d.RequestMerge = func(context.Context, string, int) error { merges++; mergedOnce = true; return nil }
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "approved-head", nil); err == nil || !retryableMergeError(err) || merges != 0 {
		t.Fatalf("store failure returned %v merges=%d", err, merges)
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "approved-head", nil); err != nil || merges != 1 {
		t.Fatalf("retry after a store failure: %v merges=%d", err, merges)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged {
		t.Fatalf("store-failure retry state=%s", current.State)
	}

	ctx, st, task, d = approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "approved-head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		return githubtrigger.StatusError(405, errors.New("required status check is expected"))
	}
	if err := d.ApplyChecksDelivery(ctx, "acme/app", "approved-head", nil); err != nil {
		t.Fatalf("405 refusal returned %v", err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "merge.failed"); count != 1 {
		t.Fatalf("405 refusal merge.failed=%d", count)
	}
}
