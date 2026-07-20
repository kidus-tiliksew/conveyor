package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func TestGitHubIssuePublicationFirstAttemptPersistsLifecycleWithoutRetryActivity(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := githubIssuePublicationFixture(t)
	worker.dispatcher.PublishIssue = func(publishCtx context.Context, publication githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		if !publication.AllowCreate {
			t.Fatal("first publication did not permit create")
		}
		if err := publication.BeforeCreate(publishCtx); err != nil {
			t.Fatal(err)
		}
		return githubtrigger.IssuePublicationResult{Number: 42, URL: "https://github.com/acme/app/issues/42"}, nil
	}

	if err := worker.Work(ctx, githubIssuePublicationJob(taskID, 1)); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("lifecycle ok=%t err=%v", ok, err)
	}
	if lifecycle.State != core.GitHubPublicationPublished || lifecycle.CreateState != core.GitHubCreateConfirmed || lifecycle.Attempts != 1 || lifecycle.CreateAttempts != 1 {
		t.Fatalf("lifecycle=%+v", lifecycle)
	}
	if count, countErr := st.CountEvents(ctx, taskID, "github_issue.publication_retry"); countErr != nil || count != 0 {
		t.Fatalf("retry events=%d err=%v", count, countErr)
	}
	if count, countErr := st.CountEvents(ctx, taskID, "github_issue.publication_published"); countErr != nil || count != 1 {
		t.Fatalf("published events=%d err=%v", count, countErr)
	}
}

func TestGitHubIssuePublicationRecoverableFailureEmitsRetryActivityWithError(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := githubIssuePublicationFixture(t)
	wantErr := errors.New("GitHub create acknowledgement was lost")
	worker.dispatcher.PublishIssue = func(publishCtx context.Context, publication githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		if err := publication.BeforeCreate(publishCtx); err != nil {
			t.Fatal(err)
		}
		return githubtrigger.IssuePublicationResult{}, wantErr
	}

	if err := worker.Work(ctx, githubIssuePublicationJob(taskID, 1)); !errors.Is(err, wantErr) {
		t.Fatalf("publication error=%v", err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("lifecycle ok=%t err=%v", ok, err)
	}
	if lifecycle.State != core.GitHubPublicationRetrying || lifecycle.CreateState != core.GitHubCreateReconciling || lifecycle.Attempts != 1 || lifecycle.CreateAttempts != 1 || lifecycle.LastError != wantErr.Error() {
		t.Fatalf("lifecycle=%+v", lifecycle)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, event := range events {
		if event.Kind != "github_issue.publication_retry" {
			continue
		}
		retries++
		var payload core.GitHubLifecycle
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.LastError != wantErr.Error() {
			t.Fatalf("retry last_error=%q", payload.LastError)
		}
	}
	if retries != 1 {
		t.Fatalf("retry events=%d, want 1", retries)
	}
}

func githubIssuePublicationFixture(t *testing.T) (context.Context, store.Store, *githubIssuePublicationWorker, string) {
	t.Helper()
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "github-publication-" + core.NewTaskID(), Workspace: "test", Repo: "app", Title: "Publish issue", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: spec.Version}); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	return ctx, st, &githubIssuePublicationWorker{dispatcher: dispatcher}, task.ID
}

func githubIssuePublicationJob(taskID string, attempt int) *river.Job[queueargs.GitHubIssuePublicationArgs] {
	return &river.Job[queueargs.GitHubIssuePublicationArgs]{
		JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt, MaxAttempts: 5},
		Args:   queueargs.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: taskID},
	}
}

func TestDispatchDuplicateWithActiveConflictFixIsAcknowledged(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := dispatchFailureFixture(t, true)
	duplicate := &pgconn.PgError{Code: "23505", ConstraintName: "jobs_pkey"}

	if err := worker.handleFailure(ctx, dispatchTaskJob(taskID, 1, 5), duplicate); err != nil {
		t.Fatalf("duplicate recovery error = %v", err)
	}
	current, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != core.TaskRunning || current.NextStage != core.StageImplement {
		t.Fatalf("task after idempotent recovery = %+v", current)
	}
	if count, countErr := st.CountEvents(ctx, taskID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed events = %d, err = %v", count, countErr)
	}
}

func TestDispatchGenuineFailureStillRequeues(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := dispatchFailureFixture(t, false)
	wantErr := errors.New("database unavailable")

	if err := worker.handleFailure(ctx, dispatchTaskJob(taskID, 1, 5), wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("failure error = %v, want %v", err, wantErr)
	}
	current, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("task after genuine failure = %+v", current)
	}
	if count, countErr := st.CountEvents(ctx, taskID, "dispatch.failed"); countErr != nil || count != 1 {
		t.Fatalf("dispatch.failed events = %d, err = %v", count, countErr)
	}
}

func dispatchFailureFixture(t *testing.T, withConflictFix bool) (context.Context, store.Store, *dispatchTaskWorker, string) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	taskID := "dispatch-failure-" + core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if withConflictFix {
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict"}); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher := New(st, &config.Config{Workspace: "test"}, nil)
	return ctx, st, &dispatchTaskWorker{dispatcher: dispatcher}, taskID
}

func dispatchTaskJob(taskID string, attempt, maxAttempts int) *river.Job[queueargs.DispatchTaskArgs] {
	return &river.Job[queueargs.DispatchTaskArgs]{
		JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt, MaxAttempts: maxAttempts},
		Args:   queueargs.DispatchTaskArgs{WorkspaceID: "test", TaskID: taskID},
	}
}
