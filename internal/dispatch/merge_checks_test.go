package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func init() {
	// A fixture that stubs only ViewPullRequest never reads a real forge. The
	// real reader has its own test below.
	newCommitChecksReader = func(*Dispatcher) func(context.Context, string, string) (github.CheckStatus, error) {
		return NoChecksReportedForTest
	}
}

// New wires the real reader through the workspace App, so a workspace with no
// connected App reads as refused and never as none reported
// (req-delivery-and-forge AC-1.8).
func TestRealCommitChecksReaderRefusesWithoutWorkspaceApp(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	d := New(store.NewMemory(), &config.Config{Workspace: "test"}, nil)
	_, err := realCommitChecksReader(d)(ctx, "acme/app", "abc")
	if github.ErrorCategory(err) != github.ForgePermission {
		t.Fatalf("err = %v, want the permission category", err)
	}
}

func TestRetryableMergeErrorClassifiesCheckStatus(t *testing.T) {
	transient := github.StatusError(502, errors.New("bad gateway"))
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"held":                {ErrCheckStatusHeld, false},
		"pending":             {ErrCheckStatusPending, false},
		"unread":              {ErrCheckStatusUnread, true},
		"unread with a cause": {errors.Join(ErrCheckStatusUnread, transient), true},
	} {
		if got := retryableMergeError(tc.err); got != tc.want {
			t.Errorf("%s = %t, want %t", name, got, tc.want)
		}
	}
}

// A check delivery treats a hold and a wait as handled, and hands an unanswered
// read to the webhook applier's retry (req-quiet-idle-runtime AC-3.1).
func TestApplyChecksDeliveryAppliesCheckStatus(t *testing.T) {
	open := github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}
	for name, tc := range map[string]struct {
		read      github.CheckStatus
		readErr   error
		retryable bool
	}{
		"failed":     {read: github.CheckStatus{Checks: []github.Check{{Name: "e2e", State: github.CheckFailed}}}},
		"pending":    {read: github.CheckStatus{Checks: []github.Check{{Name: "e2e", State: github.CheckPending}}}},
		"permission": {readErr: github.PermissionError(errors.New("resource not accessible by integration"))},
		"transient":  {readErr: github.StatusError(502, errors.New("bad gateway")), retryable: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixture(t, "acme/app")
			if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
				t.Fatal(err)
			}
			merges := 0
			d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) { return open, nil }
			d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
			d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) { return tc.read, tc.readErr }
			err := d.ApplyChecksDelivery(ctx, "acme/app", "head", nil)
			if tc.retryable != (err != nil) || (err != nil && !errors.Is(err, ErrCheckStatusUnread)) {
				t.Fatalf("err = %v, retryable = %t", err, tc.retryable)
			}
			if merges != 0 {
				t.Fatalf("merges = %d", merges)
			}
			for _, kind := range mergeEventKinds(t, st, ctx, task.ID) {
				if kind == "merge.requested" || kind == "merge.failed" {
					t.Fatalf("recorded %s", kind)
				}
			}
		})
	}
}

// The sweep's timed pass stops at a rate-limited check-status read as it stops
// at a rate-limited pull-request read (component-runtime, Timed GitHub fallback).
func TestRateLimitedCheckReadStopsTheTimedSweep(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	limited := &github.Error{Category: github.ForgeRateLimited, RetryAt: time.Now().Add(time.Minute), Err: errors.New("rate limited")}
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head"}, nil
	}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		return github.CheckStatus{}, limited
	}
	_, err := d.reconcileMergeReadiness(ctx, true)
	if !rateLimited(err) {
		t.Fatalf("err = %v, want the rate-limited answer", err)
	}
}

// A hold, a wait, and an unanswered read leave the verdict accepted: the caller
// of the verdict gets nil, and the merge stays recorded (the applyReview path).
func TestApprovingVerdictSurvivesCheckStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		read    github.CheckStatus
		readErr error
		kind    string
	}{
		"failed":    {read: github.CheckStatus{Checks: []github.Check{{Name: "e2e", State: github.CheckFailed}}}, kind: store.EventCheckHold},
		"pending":   {read: github.CheckStatus{Checks: []github.Check{{Name: "e2e", State: github.CheckPending}}}, kind: store.EventCheckPending},
		"transient": {readErr: github.StatusError(502, errors.New("bad gateway")), kind: store.EventCheckPending},
		"refused":   {readErr: github.PermissionError(errors.New("refused")), kind: store.EventCheckHold},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
			st := store.NewMemory()
			task := core.Task{ID: "verdict-" + name, Workspace: "test", Repo: "app", Branch: "conveyor/verdict", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer"}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, BaselineSHA: "head"}
			if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"head_sha": "head"})}); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
			merges := 0
			d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
				return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
			}
			d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
			d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) { return tc.read, tc.readErr }
			review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "approved"}
			if err := d.ApplyExternalReviewPinned(ctx, task, job, review, order.ID, "review-session", "reviewer", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
				t.Fatalf("verdict caller got %v", err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != core.TaskApproved || merges != 0 {
				t.Fatalf("task=%+v merges=%d err=%v", current, merges, err)
			}
			events, err := st.ListCheckStatusEvents(ctx, []string{task.ID})
			if err != nil || len(events[task.ID]) == 0 || events[task.ID][len(events[task.ID])-1].Kind != tc.kind {
				t.Fatalf("events=%+v err=%v, want %s last", events[task.ID], err, tc.kind)
			}
		})
	}
}

func failingCheckReader(t *testing.T) func(context.Context, string, string) (github.CheckStatus, error) {
	return func(context.Context, string, string) (github.CheckStatus, error) {
		t.Error("the act read check status")
		return github.CheckStatus{}, errors.New("no read expected")
	}
}

// The gate merge reads no check status and refuses nothing because of it
// (req-review-gates-evidence AC-1.8).
func TestGateMergeReadsNoCheckStatus(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-no-checks")
	d.ReadCommitChecks = failingCheckReader(t)
	merged, err := d.MergeAtGate(ctx, task, gateOperator)
	if err != nil || merged.State != core.TaskMerged || forge.merges != 1 {
		t.Fatalf("merged=%+v merges=%d err=%v", merged, forge.merges, err)
	}
	if events, _ := st.ListCheckStatusEvents(ctx, []string{task.ID}); len(events[task.ID]) != 0 {
		t.Fatalf("check-status events = %+v", events)
	}
}

// With the merge gate on, the sweep and an approved task's merge act read none.
func TestGateOnApprovedTaskReadsNoCheckStatus(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: gateOperator.ID, ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	d.ReadCommitChecks = failingCheckReader(t)
	merged := false
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		if merged {
			return github.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merged = true; return nil }
	if _, err := d.reconcileMergeReadiness(ctx, false); err != nil {
		t.Fatal(err)
	}
	if merged {
		t.Fatal("the sweep merged a gate-on task")
	}
	if err := d.MergeApprovedTask(ctx, task, gateOperator); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged {
		t.Fatalf("state = %s", current.State)
	}
}

// The feedback's scenario end to end: the reviewer's verdict stands, the first
// read fails transiently, and the job alone merges the task. No webhook,
// restart, timed fallback, or deadline runs (req-review-gates-evidence AC-1.7).
func TestApprovingVerdictMergesThroughTheRetryJob(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewVolatileBackend()
	task := core.Task{ID: "verdict-retry", Workspace: "test", Repo: "app", Branch: "conveyor/verdict-retry", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer"}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, BaselineSHA: "head"}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"head_sha": "head"})}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	merged := make(chan struct{})
	merges, reads := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		if merges > 0 {
			return github.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; close(merged); return nil }
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		reads++
		if reads == 1 {
			return github.CheckStatus{}, github.StatusError(502, errors.New("bad gateway"))
		}
		return github.CheckStatus{Checks: []github.Check{{Name: "build", State: github.CheckPassed}}}, nil
	}

	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "approved"}
	if err := d.ApplyExternalReviewPinned(ctx, task, job, review, order.ID, "review-session", "reviewer", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
		t.Fatalf("verdict caller got %v", err)
	}
	if current, err := st.GetTask(ctx, task.ID); err != nil || current.State != core.TaskApproved || merges != 0 {
		t.Fatalf("task=%+v merges=%d err=%v, want approved and unmerged before the job runs", current, merges, err)
	}
	reviewEvents := func() int {
		t.Helper()
		events, err := st.ListEvents(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.Kind == "review.round_completed" || event.Kind == "review.completed" {
				count++
			}
		}
		return count
	}
	accepted := reviewEvents()
	if accepted == 0 {
		t.Fatal("the verdict recorded no review")
	}
	runtime, err := testRuntime(t, st.Log(), d, &ShutdownMarker{}, []string{"test"}, map[string]*config.Config{"test": cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := runtime.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-merged:
	case <-time.After(30 * time.Second):
		t.Fatal("the retry job did not merge")
	}
	deadline := time.After(30 * time.Second)
	for {
		current, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == core.TaskMerged {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("task = %+v, want merged", current)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if reads != 2 || merges != 1 {
		t.Fatalf("reads=%d merges=%d", reads, merges)
	}
	// The retry reads and merges; it never applies the accepted review again.
	if got := reviewEvents(); got != accepted {
		t.Fatalf("review events = %d after the job, %d at the verdict", got, accepted)
	}
}

// The volatile store has no job, so the memory queue carries the retry.
func TestFailedReadOnTheMemoryQueueRetries(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	merged := make(chan struct{})
	reads, merges := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		if merges > 0 {
			return github.PullRequest{Number: 41, State: "closed", Merged: true, HeadSHA: "head", BaseSHA: "base", MergeCommitSHA: "landed"}, nil
		}
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; close(merged); return nil }
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		reads++
		if reads == 1 {
			return github.CheckStatus{}, github.StatusError(502, errors.New("bad gateway"))
		}
		return github.CheckStatus{}, nil
	}
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusUnread) {
		t.Fatalf("err = %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); d.Run(runCtx) }()
	defer func() { stop(); <-done }()
	select {
	case <-merged:
	case <-time.After(30 * time.Second):
		t.Fatal("the memory queue did not retry")
	}
}

func checkReadWorker(t *testing.T) (context.Context, store.Store, core.Task, *dispatchTaskWorker) {
	t.Helper()
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		return github.PullRequest{Number: 41, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head", BaseSHA: "base"}, nil
	}
	d.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		return github.CheckStatus{}, github.StatusError(502, errors.New("bad gateway"))
	}
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusUnread) {
		t.Fatalf("err = %v", err)
	}
	return ctx, st, task, &dispatchTaskWorker{dispatcher: d}
}

// The check-status rows return before handleFailure: an unanswered attempt
// returns the error with no dispatch.failed and no transition, and the final
// attempt completes (component-durable-queue invariant 37).
func TestCheckReadAttemptNeverReachesHandleFailure(t *testing.T) {
	ctx, st, task, worker := checkReadWorker(t)
	transitions, err := st.CountEvents(ctx, task.ID, "task.state_changed")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < queue.DispatchTaskMaxAttempts; attempt++ {
		if err := worker.Work(ctx, dispatchTaskJob(task.ID, attempt, queue.DispatchTaskMaxAttempts)); !errors.Is(err, ErrCheckStatusUnread) {
			t.Fatalf("attempt %d err = %v, want the read's error for job.failed", attempt, err)
		}
	}
	if err := worker.Work(ctx, dispatchTaskJob(task.ID, queue.DispatchTaskMaxAttempts, queue.DispatchTaskMaxAttempts)); err != nil {
		t.Fatalf("final attempt err = %v, want completion", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "dispatch.failed"); count != 0 {
		t.Fatalf("dispatch.failed = %d", count)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "task.state_changed"); count != transitions {
		t.Fatalf("task.state_changed = %d, want %d", count, transitions)
	}
	events, err := st.ListCheckStatusEvents(ctx, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if status := store.FoldCheckStatus(events[task.ID], "head"); !status.RetriesExhausted {
		t.Fatalf("status = %+v, want the end of the retries recorded", status)
	}
	// The retries ended, so a job that finds them so reads nothing.
	reads := 0
	worker.dispatcher.ReadCommitChecks = func(context.Context, string, string) (github.CheckStatus, error) {
		reads++
		return github.CheckStatus{}, nil
	}
	if err := worker.Work(ctx, dispatchTaskJob(task.ID, 1, queue.DispatchTaskMaxAttempts)); err != nil || reads != 0 {
		t.Fatalf("err=%v reads=%d", err, reads)
	}
}

// A task the rows don't apply to behaves as before: a queued task's real dispatch
// failure still reaches dispatch.failed and its transition.
func TestRealDispatchFailureStillReachesHandleFailure(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "queued-failure", Workspace: "test", Repo: "app", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test"}, nil)
	worker := &dispatchTaskWorker{dispatcher: d}
	if err := worker.Work(ctx, dispatchTaskJob(task.ID, 1, queue.DispatchTaskMaxAttempts)); err == nil {
		t.Fatal("a stage with no route dispatched")
	}
	if count, err := st.CountEvents(ctx, task.ID, "dispatch.failed"); err != nil || count != 1 {
		t.Fatalf("dispatch.failed = %d, err = %v", count, err)
	}
	if count, err := st.CountEvents(ctx, task.ID, "task.state_changed"); err != nil || count != 1 {
		t.Fatalf("task.state_changed = %d, err = %v", count, err)
	}
}

// With the merge gate on, the rows don't apply: the job reads no check status.
func TestCheckReadRowsSkipAGateOnTask(t *testing.T) {
	ctx, _, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	d.ReadCommitChecks = failingCheckReader(t)
	worker := &dispatchTaskWorker{dispatcher: d}
	if err := worker.Work(ctx, dispatchTaskJob(task.ID, 1, queue.DispatchTaskMaxAttempts)); err != nil {
		t.Fatalf("err = %v", err)
	}
}
