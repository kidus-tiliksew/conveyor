package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const rescueReason = "rescued: claim exceeded 1h0m0s"

func requireFailFinal(t *testing.T, st store.Store, ctx context.Context, taskID string, wantBounce bool) {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	if err != nil || task.State != core.TaskParked || task.RecoveryStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var failed, bounced, final int
	for _, event := range events {
		switch {
		case event.Kind == "dispatch.failed":
			failed++
			if !strings.Contains(string(event.Payload), rescueReason) {
				t.Fatalf("dispatch.failed payload=%s", event.Payload)
			}
		case event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"stage.bounce"`):
			bounced++
		case event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"dispatch.fail_final"`):
			final++
		}
	}
	wantBounces := 0
	if wantBounce {
		wantBounces = 1
	}
	if failed != 1 || bounced != wantBounces || final != 1 {
		t.Fatalf("dispatch.failed=%d stage.bounce=%d dispatch.fail_final=%d, want 1/%d/1", failed, bounced, final, wantBounces)
	}
}

// A dispatch_task job that rescue discards reaches the final-attempt stop of
// the dispatch outcome table: a running task bounces and parks, a queued task
// parks instead of waiting to be re-enqueued with fresh attempts, and a
// repeat changes nothing (req-task-lifecycle-and-queue AC-6.2;
// component-durable-queue, Handlers).
func TestDiscardedDispatchJobParksTheTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state core.TaskState
	}{{"Running", core.TaskRunning}, {"Queued", core.TaskQueued}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
			st := store.NewMemory()
			taskID := "discard-" + core.NewTaskID()
			if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "test", Repo: "app", State: tc.state, NextStage: core.StageImplement, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			worker := &dispatchTaskWorker{dispatcher: New(st, &config.Config{Workspace: "test"}, nil)}
			job := dispatchTaskJob(taskID, queue.DispatchTaskMaxAttempts, queue.DispatchTaskMaxAttempts)
			if err := worker.Discarded(ctx, job, rescueReason); err != nil {
				t.Fatal(err)
			}
			requireFailFinal(t, st, ctx, taskID, tc.state == core.TaskRunning)
			if err := worker.Discarded(ctx, job, rescueReason); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			requireFailFinal(t, st, ctx, taskID, tc.state == core.TaskRunning)
		})
	}
}

// A queued task whose stage a work order holds is waiting for a claimant,
// not stuck, so a discarded dispatch job leaves it alone.
func TestDiscardedDispatchJobLeavesAHeldTask(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	taskID := "discard-held-" + core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "test", Repo: "app", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	worker := &dispatchTaskWorker{dispatcher: New(st, &config.Config{Workspace: "test"}, nil)}
	if err := worker.Discarded(ctx, dispatchTaskJob(taskID, queue.DispatchTaskMaxAttempts, queue.DispatchTaskMaxAttempts), rescueReason); err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil || task.State != core.TaskQueued {
		t.Fatalf("held task=%+v err=%v", task, err)
	}
	if count, countErr := st.CountEvents(ctx, taskID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed=%d err=%v", count, countErr)
	}
}

// Publication rows whose job rescue discarded become failed with the rescue
// reason, so the bounded automatic re-enqueue finds them
// (req-bounded-automatic-recovery AC-4.1).
func TestDiscardedPublicationJobsMarkTheRowFailed(t *testing.T) {
	t.Parallel()
	t.Run("Review", func(t *testing.T) {
		t.Parallel()
		ctx, st, worker, publication := reviewPublicationFixture(t, "approve")
		if err := worker.Discarded(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 5), rescueReason); err != nil {
			t.Fatal(err)
		}
		stored, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
		if err != nil || stored.State != core.ReviewPublicationFailed || stored.LastError != rescueReason {
			t.Fatalf("publication=%+v err=%v", stored, err)
		}
		if err = worker.Discarded(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 5), "again"); err != nil {
			t.Fatal(err)
		}
		if count, countErr := st.CountEvents(ctx, publication.TaskID, "review.publication_failed"); countErr != nil || count != 1 {
			t.Fatalf("review.publication_failed=%d err=%v", count, countErr)
		}
	})
	t.Run("GitHubIssue", func(t *testing.T) {
		t.Parallel()
		ctx, st, worker, taskID := githubIssuePublicationFixture(t)
		if err := worker.Discarded(ctx, githubIssuePublicationJob(taskID, 5), rescueReason); err != nil {
			t.Fatal(err)
		}
		lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
		if err != nil || lifecycle.State != core.GitHubPublicationFailed || lifecycle.LastError != rescueReason {
			t.Fatalf("lifecycle=%+v err=%v", lifecycle, err)
		}
		if err = worker.Discarded(ctx, githubIssuePublicationJob(taskID, 5), "again"); err != nil {
			t.Fatal(err)
		}
		if count, countErr := st.CountEvents(ctx, taskID, "github_issue.publication_failed"); countErr != nil || count != 1 {
			t.Fatalf("github_issue.publication_failed=%d err=%v", count, countErr)
		}
	})
}

// failingTaskStore fails GetTask, the early return a publication attempt
// takes after it records retrying.
type failingTaskStore struct {
	store.Store
	err error
}

func (s failingTaskStore) GetTask(context.Context, string) (core.Task, error) {
	return core.Task{}, s.err
}

// A publication attempt that returns before its forge call keeps the row
// retrying on an earlier attempt and marks it failed on the last one, for the
// review and GitHub issue workers (component-durable-queue, Handlers;
// req-bounded-automatic-recovery AC-4.1).
func TestPublicationEarlyReturnOnLastAttemptMarksFailed(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("task read unavailable")
	t.Run("ReviewTaskRead", func(t *testing.T) {
		t.Parallel()
		ctx, st, worker, publication := reviewPublicationFixture(t, "approve")
		worker.dispatcher.Store = failingTaskStore{Store: st, err: wantErr}
		if err := worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 1)); !errors.Is(err, wantErr) {
			t.Fatalf("first attempt err=%v", err)
		}
		stored, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
		if err != nil || stored.State != core.ReviewPublicationRetrying || stored.LastError != wantErr.Error() {
			t.Fatalf("after first attempt=%+v err=%v", stored, err)
		}
		if err = worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 5)); !errors.Is(err, wantErr) {
			t.Fatalf("last attempt err=%v", err)
		}
		stored, err = st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
		if err != nil || stored.State != core.ReviewPublicationFailed || stored.LastError != wantErr.Error() {
			t.Fatalf("after last attempt=%+v err=%v", stored, err)
		}
	})
	t.Run("ReviewRepositoryLookup", func(t *testing.T) {
		t.Parallel()
		ctx, st, worker, publication := reviewPublicationFixture(t, "approve")
		worker.dispatcher = New(st, &config.Config{Workspace: "test"}, nil)
		err := worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 5))
		if err == nil || !strings.Contains(err.Error(), "GitHub repository is not configured") {
			t.Fatalf("last attempt err=%v", err)
		}
		stored, getErr := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
		if getErr != nil || stored.State != core.ReviewPublicationFailed || stored.LastError != err.Error() {
			t.Fatalf("after last attempt=%+v err=%v", stored, getErr)
		}
	})
	t.Run("GitHubIssueTaskRead", func(t *testing.T) {
		t.Parallel()
		ctx, st, worker, taskID := githubIssuePublicationFixture(t)
		worker.dispatcher.PublishIssue = func(context.Context, githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
			t.Fatal("publication reached the forge")
			return githubtrigger.IssuePublicationResult{}, nil
		}
		worker.dispatcher.Store = failingTaskStore{Store: st, err: wantErr}
		if err := worker.Work(ctx, githubIssuePublicationJob(taskID, 1)); !errors.Is(err, wantErr) {
			t.Fatalf("first attempt err=%v", err)
		}
		lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
		if err != nil || lifecycle.State != core.GitHubPublicationRetrying || lifecycle.LastError != wantErr.Error() {
			t.Fatalf("after first attempt=%+v err=%v", lifecycle, err)
		}
		if err = worker.Work(ctx, githubIssuePublicationJob(taskID, 5)); !errors.Is(err, wantErr) {
			t.Fatalf("last attempt err=%v", err)
		}
		lifecycle, _, err = st.GetGitHubLifecycle(ctx, taskID)
		if err != nil || lifecycle.State != core.GitHubPublicationFailed || lifecycle.LastError != wantErr.Error() {
			t.Fatalf("after last attempt=%+v err=%v", lifecycle, err)
		}
	})
}

// The daemon registers a discard handler for dispatch_task and both forge
// publication kinds, so rescue reaches their stop; other kinds keep none.
func TestRegistrationsBindDiscardHandlers(t *testing.T) {
	t.Parallel()
	d := New(store.NewMemory(), &config.Config{Workspace: "test"}, nil)
	want := map[string]bool{
		queue.DispatchTaskArgs{}.Kind():           true,
		queue.ReviewPublicationArgs{}.Kind():      true,
		queue.GitHubIssuePublicationArgs{}.Kind(): true,
	}
	for _, registration := range d.Registrations(&ShutdownMarker{}) {
		if got := registration.Discarded != nil; got != want[registration.Kind] {
			t.Errorf("%s discard handler bound=%t, want %t", registration.Kind, got, want[registration.Kind])
		}
	}
}

// flakyStore fails the first calls of chosen store operations, as a transient
// outage does.
type flakyStore struct {
	store.Store
	// getTaskID limits GetTask failures to one task, when set.
	getTaskID                          string
	getTask, updateReview, updateIssue atomic.Int32
	failCommand                        core.TaskCommand
	commandFailures                    atomic.Int32
}

var errStoreOutage = errors.New("store unavailable")

func (s *flakyStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	if (s.getTaskID == "" || s.getTaskID == id) && s.getTask.Add(-1) >= 0 {
		return core.Task{}, errStoreOutage
	}
	return s.Store.GetTask(ctx, id)
}

func (s *flakyStore) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if s.updateReview.Add(-1) >= 0 {
		return errStoreOutage
	}
	return s.Store.UpdateReviewPublication(ctx, publication)
}

func (s *flakyStore) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	if s.updateIssue.Add(-1) >= 0 {
		return errStoreOutage
	}
	return s.Store.UpdateGitHubLifecycle(ctx, lifecycle)
}

func (s *flakyStore) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, cmd taskops.Command) (core.Task, error) {
	if cmd.Kind == s.failCommand && s.commandFailures.Add(-1) >= 0 {
		return core.Task{}, errStoreOutage
	}
	return s.Store.ApplyTaskCommand(ctx, lease, id, cmd)
}

// stuckJobLog holds one job claimed by a dead worker on its last attempt.
func stuckJobLog(t *testing.T, kind, key string, args any, maxAttempts int) (*logqueue.NotifyingLog, eventlog.StreamID) {
	t.Helper()
	log := logqueue.Notify(memlog.New(), changes.NewHub())
	claimedAt := time.Now().UTC().Add(-72 * time.Hour)
	if _, err := logqueue.Enqueue(context.Background(), log, "test", kind, key, args, maxAttempts, claimedAt); err != nil {
		t.Fatal(err)
	}
	stream := logqueue.StreamFor(kind, key)
	claim := fmt.Sprintf(`{"attempt":%d,"worker":"dead","claimed_at":%q}`, maxAttempts, claimedAt.Format(time.RFC3339Nano))
	if _, err := log.Append(context.Background(), "test", stream, 1, []eventlog.NewEvent{{Kind: logqueue.KindClaimed, Payload: []byte(claim), At: claimedAt}}); err != nil {
		t.Fatal(err)
	}
	return log, stream
}

// startDiscardRuntimes starts the daemon's registrations on replicas of one log.
func startDiscardRuntimes(t *testing.T, log eventlog.Store, d *Dispatcher, replicas int) {
	t.Helper()
	configs := map[string]*config.Config{"test": {Workspace: "test"}}
	for range replicas {
		rt, err := testRuntime(t, log, d, &ShutdownMarker{}, []string{"test"}, configs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = rt.StopAndCancel(ctx)
		})
	}
}

func waitForDiscard(t *testing.T, log eventlog.Store, stream eventlog.StreamID, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := logqueue.Load(context.Background(), log, "test", stream)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == logqueue.StateDiscarded && done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the discard was not finalized")
}

func requireNoJobNotice(t *testing.T, notices *changes.Subscription) {
	t.Helper()
	select {
	case n := <-notices.C():
		t.Fatalf("recovery depended on notice %+v", n)
	default:
	}
}

// When the first store operation of the discard handler fails, the task still
// parks while the daemon stays up, with no notice and no restart, and across
// replicas each final transition and its evidence are recorded once
// (req-task-lifecycle-and-queue AC-6.2; component-durable-queue, Handlers).
func TestDiscardedDispatchRecoversFromAStoreFailureWithoutANotice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    core.TaskState
		replicas int
	}{
		{"RunningOneReplica", core.TaskRunning, 1},
		{"QueuedOneReplica", core.TaskQueued, 1},
		{"RunningTwoReplicas", core.TaskRunning, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
			inner := store.NewMemory()
			taskID := "discard-recovery-" + core.NewTaskID()
			if err := inner.CreateTask(ctx, core.Task{ID: taskID, Workspace: "test", Repo: "app", State: tc.state, NextStage: core.StageImplement, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			st := &flakyStore{Store: inner}
			st.getTask.Store(1)
			kind := queue.DispatchTaskArgs{}.Kind()
			log, stream := stuckJobLog(t, kind, taskID, queue.DispatchTaskArgs{WorkspaceID: "test", TaskID: taskID}, queue.DispatchTaskMaxAttempts)
			notices := log.Changes().Subscribe("test", changes.Job)
			defer notices.Close()
			startDiscardRuntimes(t, log, New(st, &config.Config{Workspace: "test"}, nil), tc.replicas)
			waitForDiscard(t, log, stream, func() bool {
				task, err := inner.GetTask(ctx, taskID)
				return err == nil && task.State == core.TaskParked
			})
			if st.getTask.Load() >= 0 {
				t.Fatal("the failure was not injected")
			}
			requireNoJobNotice(t, notices)
			events, err := inner.ListEvents(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			var failed, bounced, final int
			for _, event := range events {
				switch {
				case event.Kind == "dispatch.failed":
					failed++
				case event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"stage.bounce"`):
					bounced++
				case event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"dispatch.fail_final"`):
					final++
				}
			}
			wantBounces := 0
			if tc.state == core.TaskRunning {
				wantBounces = 1
			}
			if failed != 1 || bounced != wantBounces || final != 1 {
				t.Fatalf("dispatch.failed=%d stage.bounce=%d dispatch.fail_final=%d, want 1/%d/1", failed, bounced, final, wantBounces)
			}
			discards := 0
			logEvents, err := log.Read(context.Background(), "test", stream, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range logEvents {
				if event.Kind == logqueue.KindDiscarded {
					discards++
				}
			}
			if discards != 1 {
				t.Fatalf("job.discarded=%d", discards)
			}
		})
	}
}

// When a publication row's first failed-state write fails, the row still
// becomes failed with no notice and no restart, and the failure is recorded
// once, so the bounded automatic re-enqueue finds it
// (req-bounded-automatic-recovery AC-4.1).
func TestDiscardedPublicationRecoversFromAStoreFailureWithoutANotice(t *testing.T) {
	t.Parallel()
	t.Run("Review", func(t *testing.T) {
		t.Parallel()
		ctx, inner, worker, publication := reviewPublicationFixture(t, "approve")
		st := &flakyStore{Store: inner}
		st.updateReview.Store(1)
		worker.dispatcher.Store = st
		log, stream := stuckJobLog(t, queue.ReviewPublicationArgs{}.Kind(), publication.ReviewWorkOrderID, queue.ReviewPublicationArgs{WorkspaceID: "test", ReviewWorkOrderID: publication.ReviewWorkOrderID}, 5)
		notices := log.Changes().Subscribe("test", changes.Job)
		defer notices.Close()
		startDiscardRuntimes(t, log, worker.dispatcher, 1)
		waitForDiscard(t, log, stream, func() bool {
			stored, err := inner.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
			return err == nil && stored.State == core.ReviewPublicationFailed
		})
		requireNoJobNotice(t, notices)
		if count, err := inner.CountEvents(ctx, publication.TaskID, "review.publication_failed"); err != nil || count != 1 {
			t.Fatalf("review.publication_failed=%d err=%v", count, err)
		}
	})
	t.Run("GitHubIssue", func(t *testing.T) {
		t.Parallel()
		ctx, inner, worker, taskID := githubIssuePublicationFixture(t)
		st := &flakyStore{Store: inner}
		st.updateIssue.Store(1)
		worker.dispatcher.Store = st
		log, stream := stuckJobLog(t, queue.GitHubIssuePublicationArgs{}.Kind(), taskID, queue.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: taskID}, 5)
		notices := log.Changes().Subscribe("test", changes.Job)
		defer notices.Close()
		startDiscardRuntimes(t, log, worker.dispatcher, 1)
		waitForDiscard(t, log, stream, func() bool {
			lifecycle, ok, err := inner.GetGitHubLifecycle(ctx, taskID)
			return err == nil && ok && lifecycle.State == core.GitHubPublicationFailed
		})
		requireNoJobNotice(t, notices)
		if count, err := inner.CountEvents(ctx, taskID, "github_issue.publication_failed"); err != nil || count != 1 {
			t.Fatalf("github_issue.publication_failed=%d err=%v", count, err)
		}
	})
}

// A retry after any failed step applies each final transition and the
// dispatch.failed evidence once, including when the handler's own last attempt
// failed before the discard handler ran.
func TestDispatchFinalStepsApplyOnceAcrossRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		command core.TaskCommand
		first   func(*dispatchTaskWorker, context.Context, queue.Job) error
	}{
		{"DiscardBounceFails", core.TaskStageBounce, func(w *dispatchTaskWorker, ctx context.Context, job queue.Job) error {
			return w.Discarded(ctx, job, rescueReason)
		}},
		{"DiscardParkFails", core.TaskDispatchFailFinal, func(w *dispatchTaskWorker, ctx context.Context, job queue.Job) error {
			return w.Discarded(ctx, job, rescueReason)
		}},
		{"LastAttemptParkFails", core.TaskDispatchFailFinal, func(w *dispatchTaskWorker, ctx context.Context, job queue.Job) error {
			return w.handleFailure(ctx, job, errors.New(rescueReason))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, inner, worker, taskID := dispatchFailureFixture(t, false)
			st := &flakyStore{Store: inner, failCommand: tc.command}
			st.commandFailures.Store(1)
			worker.dispatcher.Store = st
			job := dispatchTaskJob(taskID, queue.DispatchTaskMaxAttempts, queue.DispatchTaskMaxAttempts)
			if err := tc.first(worker, ctx, job); !errors.Is(err, errStoreOutage) {
				t.Fatalf("first err=%v", err)
			}
			if err := worker.Discarded(ctx, job, rescueReason); err != nil {
				t.Fatal(err)
			}
			if err := worker.Discarded(ctx, job, rescueReason); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			requireFailFinal(t, inner, ctx, taskID, true)
		})
	}
}

// A task that no longer exists has nothing to finalize, so a retry ends.
func TestDiscardedDispatchForAMissingTaskEnds(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	worker := &dispatchTaskWorker{dispatcher: New(store.NewMemory(), &config.Config{Workspace: "test"}, nil)}
	if err := worker.Discarded(ctx, dispatchTaskJob("missing", queue.DispatchTaskMaxAttempts, queue.DispatchTaskMaxAttempts), rescueReason); err != nil {
		t.Fatal(err)
	}
}
