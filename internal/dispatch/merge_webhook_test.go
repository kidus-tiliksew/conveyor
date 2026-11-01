package dispatch

import (
	"context"
	"strings"
	"sync"
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
