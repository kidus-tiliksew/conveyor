package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The pass runs in the order clock handler, and its failure reaches the alarm
// without excusing another task's deadline (component-durable-queue invariants
// 32, 33).
func TestOrderClockRunsTheCheckStatusDeadlinePass(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	second := core.Task{ID: "second-task", Workspace: "test", Repo: "app", Branch: "conveyor/second-task", State: core.TaskApproved, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, second.ID, "second-head"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	d.Now = func() time.Time { return now }
	due := now.Add(-time.Minute)
	for _, row := range []struct{ id, head string }{{task.ID, "head"}, {second.ID, "second-head"}} {
		event := core.Event{TaskID: row.id, Kind: store.EventCheckPending, Payload: core.JSONPayload(map[string]any{"head_sha": row.head, "reason": "pending"})}
		if _, err := st.RecordCheckStatusDeadline(ctx, row.id, row.head, due, event); err != nil {
			t.Fatal(err)
		}
	}
	reads := map[string]int{}
	d.ReadCommitChecks = func(_ context.Context, _, sha string) (github.CheckStatus, error) {
		reads[sha]++
		return github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPending}}}, nil
	}
	refused := errors.New("store refused the fold")
	d.Store = foldRefusingStore{Store: st, refuse: task.ID, err: refused}
	worker := &orderClockWorker{dispatcher: d}
	err := worker.Work(ctx, orderClockJob())
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the failed task's error for the alarm", err)
	}
	if reads["head"] != 1 || reads["second-head"] != 1 {
		t.Fatalf("reads = %v, want one per task", reads)
	}
	current, err := st.GetTask(ctx, second.ID)
	if err != nil || current.CheckStatusDeadline != nil {
		t.Fatalf("second task deadline = %v err = %v, want it fired", current.CheckStatusDeadline, err)
	}
	if current, err = st.GetTask(ctx, task.ID); err != nil || current.CheckStatusDeadline == nil {
		t.Fatalf("failed task deadline = %v err = %v, want it still due", current.CheckStatusDeadline, err)
	}
	d.Store = st
	if err := worker.Work(ctx, orderClockJob()); err != nil {
		t.Fatal(err)
	}
	if reads["head"] != 2 || reads["second-head"] != 1 {
		t.Fatalf("reads = %v, want the failed task read again and the fired one not", reads)
	}
}

func orderClockJob() queue.Job {
	return testJob(queue.OrderClockArgs{WorkspaceID: "test"}, 1, 1, 1)
}

type foldRefusingStore struct {
	store.Store
	refuse string
	err    error
}

func (s foldRefusingStore) ListCheckStatusEvents(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	for _, id := range ids {
		if id == s.refuse {
			return nil, s.err
		}
	}
	return s.Store.ListCheckStatusEvents(ctx, ids)
}

// Only an approved task with the merge gate off holds a deadline the pass fires
// (component-durable-queue invariant 34; component-submission-merge invariant 58).
func TestCheckStatusDeadlinePassSkipsAGateOnTask(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", "", true)
	due := time.Now().UTC().Add(-time.Hour)
	if _, err := st.RecordCheckStatusDeadline(ctx, task.ID, "head", due, core.Event{TaskID: task.ID, Kind: store.EventCheckPending, Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		t.Fatal("a gate-on task read check status")
		return github.CheckStatus{}, nil
	}
	if err := d.ApplyDueCheckStatusDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
}

// A repository no longer configured for GitHub cannot be read, which is a failed
// read: the operator is named rather than the deadline going quiet.
func TestCheckStatusDeadlineWithNoGitHubRepositoryIsStuck(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Hour)
	if _, err := st.RecordCheckStatusDeadline(ctx, task.ID, "head", due, core.Event{TaskID: task.ID, Kind: store.EventCheckPending, Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		t.Fatal("read with no GitHub repository")
		return github.CheckStatus{}, nil
	}
	if err := d.ApplyDueCheckStatusDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := d.foldCheckStatus(ctx, task.ID, "head")
	if err != nil || status.Hold == nil || status.Hold.Reason != "stuck" || status.Hold.ReadFailure == "" {
		t.Fatalf("status = %+v err = %v", status, err)
	}
}

// lockHookStore runs before ahead of the first lock it is asked for. The hook
// itself takes the lock, so it clears before running.
type lockHookStore struct {
	store.Store
	before *func()
}

func (s lockHookStore) WithTaskSideEffectLock(ctx context.Context, id string, fn func(context.Context) error) error {
	if before := *s.before; before != nil {
		*s.before = nil
		before()
	}
	return s.Store.WithTaskSideEffectLock(ctx, id, fn)
}

// An attempt that waited on the lock while the deadline fired finds it gone: it
// reads nothing, records no end of the retries, and leaves the hold alone
// (component-submission-merge invariant 61, 62).
func TestRetryAttemptFinishingAfterTheDeadlineFiredReadsNothing(t *testing.T) {
	for name, attempt := range map[string]int{"an attempt with more to come": 1, "the final attempt": queue.DispatchTaskMaxAttempts} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, worker := checkReadWorker(t)
			d := worker.dispatcher
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			now := current.CheckStatusDeadline.Add(time.Second)
			d.Now = func() time.Time { return now }
			reads := 0
			d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
				reads++
				return github.CheckStatus{}, github.StatusError(502, errors.New("bad gateway"))
			}
			views := 0
			d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
				views++
				return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head"}, nil
			}
			fire := func() {
				if err := d.ApplyDueCheckStatusDeadlines(ctx); err != nil {
					t.Error(err)
				}
			}
			d.Store = lockHookStore{Store: st, before: &fire}
			if err := worker.Work(ctx, dispatchTaskJob(task.ID, attempt, queue.DispatchTaskMaxAttempts)); err != nil {
				t.Fatalf("attempt err = %v, want completion", err)
			}
			if reads != 1 || views != 0 {
				t.Fatalf("reads=%d views=%d, want only the deadline's read", reads, views)
			}
			kinds := mergeEventKinds(t, st, ctx, task.ID)
			holds, pendings := 0, 0
			for _, kind := range kinds {
				switch kind {
				case store.EventCheckHold:
					holds++
				case store.EventCheckPending:
					pendings++
				}
			}
			if holds != 1 || pendings != 1 {
				t.Fatalf("events = %v, want one hold and no end of the retries", kinds)
			}
		})
	}
}

// A deadline falling due while an attempt holds the lock waits for it, then
// decides from what that attempt left (component-submission-merge invariant 61).
func TestDeadlineFallingDueDuringAnAttemptWaitsForIt(t *testing.T) {
	for name, tc := range map[string]struct {
		attempt   github.CheckStatus
		wantReads int
		wantHolds int
	}{
		"the attempt finds a check pending":    {attempt: github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPending}}}, wantReads: 2, wantHolds: 1},
		"the attempt finds every check passed": {attempt: github.CheckStatus{Checks: []github.Check{{Name: "build", State: github.CheckPassed}}}, wantReads: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, worker := checkReadWorker(t)
			d := worker.dispatcher
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			now := current.CheckStatusDeadline.Add(time.Second)
			d.Now = func() time.Time { return now }
			fired := make(chan error, 1)
			var once sync.Once
			reads := 0
			d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
				reads++
				// The pass starts inside the attempt's read and blocks on the lock
				// the attempt holds. Either order to the lock gives this result,
				// since the attempt is already past its own decision.
				once.Do(func() {
					go func() { fired <- d.ApplyDueCheckStatusDeadlines(ctx) }()
				})
				return tc.attempt, nil
			}
			merges := 0
			d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
			d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
				if merges > 0 {
					return github.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "head", BaseSHA: "base"}, nil
				}
				return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
			}
			if err := worker.Work(ctx, dispatchTaskJob(task.ID, 1, queue.DispatchTaskMaxAttempts)); err != nil {
				t.Fatalf("attempt err = %v", err)
			}
			if err := <-fired; err != nil {
				t.Fatal(err)
			}
			if reads != tc.wantReads {
				t.Fatalf("reads = %d, want %d", reads, tc.wantReads)
			}
			holds := 0
			for _, kind := range mergeEventKinds(t, st, ctx, task.ID) {
				if kind == store.EventCheckHold {
					holds++
				}
			}
			if holds != tc.wantHolds {
				t.Fatalf("holds = %d, want %d", holds, tc.wantHolds)
			}
		})
	}
}

// Two fires at once read once: the second finds the deadline gone under the lock.
func TestConcurrentDeadlineFiresReadOnce(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Hour)
	if _, err := st.RecordCheckStatusDeadline(ctx, task.ID, "head", due, core.Event{TaskID: task.ID, Kind: store.EventCheckPending, Payload: core.JSONPayload(map[string]any{"head_sha": "head"})}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	reads := 0
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPending}}}, nil
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := d.ApplyDueCheckStatusDeadlines(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if reads != 1 {
		t.Fatalf("reads = %d, want 1", reads)
	}
}

// A task back at approved keeps its recorded deadline at the same head, and a
// fire in between skipped it, so the read that finds it announces it to the
// alarm instead of leaving it to an unrelated fire (req-quiet-idle-runtime
// AC-2.4).
func TestReadFindingARecordedDeadlineAnnouncesIt(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		return github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPending}}}, nil
	}
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusPending) {
		t.Fatalf("err = %v", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.CheckStatusDeadline == nil {
		t.Fatalf("deadline = %v err = %v", current.CheckStatusDeadline, err)
	}
	notifier, ok := st.(interface{ Changes() *changes.Hub })
	if !ok {
		t.Fatal("the store has no change hub")
	}
	notices := notifier.Changes().Subscribe("test", changes.Deadline)
	defer notices.Close()
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusPending) {
		t.Fatalf("err = %v", err)
	}
	select {
	case notice := <-notices.C():
		if !notice.At.Equal(*current.CheckStatusDeadline) {
			t.Fatalf("notice = %+v, want the recorded deadline %s", notice, current.CheckStatusDeadline)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no deadline notice for the recorded deadline")
	}
}

// The silence fallback stops watching a task its deadline left stuck, and starts
// again when a read finds no check pending (req-quiet-idle-runtime AC-3.8).
func TestStuckHoldFromTheDeadlineLeavesTheSilenceFallback(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	fallback := &GitHubFallback{Dispatcher: d}
	watched := func() int {
		t.Helper()
		tasks, err := fallback.watchedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(tasks)
	}
	now := time.Now().UTC()
	d.Now = func() time.Time { return now }
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		return github.StatusError(405, errors.New("merge refused"))
	}
	read := github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPending}}}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) { return read, nil }
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusPending) {
		t.Fatalf("err = %v", err)
	}
	if watched() != 1 {
		t.Fatal("a pending task is not watched")
	}
	now = now.Add(CheckStatusDeadlineWindow)
	if err := d.ApplyDueCheckStatusDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if watched() != 0 {
		t.Fatal("a stuck task is still watched")
	}
	// Still pending: the hold stays and so does the exclusion.
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusHeld) {
		t.Fatalf("err = %v", err)
	}
	if watched() != 0 {
		t.Fatal("a read that still finds a check pending ended the exclusion")
	}
	read = github.CheckStatus{Checks: []github.Check{{Name: "deploy", State: github.CheckPassed}}}
	if err := d.MergeApprovedTaskAutomatically(ctx, task); err == nil {
		t.Fatal("the scripted forge refusal did not reach the caller")
	}
	if watched() != 1 {
		t.Fatal("a read finding no check pending did not end the exclusion")
	}
}
