package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
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
