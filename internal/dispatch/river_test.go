package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
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
	assertForgeWriteAuthor(t, st, ctx, taskID, "github_issue.publication_published", core.ForgeAuthorHost)
}

func TestGitHubIssuePublicationRecoverableFailureEmitsRetryActivityWithError(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := githubIssuePublicationFixture(t)
	wantErr := errors.New("GitHub create acknowledgement was lost")
	worker.dispatcher.PublishIssue = func(publishCtx context.Context, publication githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		if err := publication.BeforeCreate(publishCtx); err != nil {
			t.Fatal(err)
		}
		return githubtrigger.IssuePublicationResult{}, &githubtrigger.Error{Category: githubtrigger.ForgeRequest, Err: wantErr}
	}

	if err := worker.Work(ctx, githubIssuePublicationJob(taskID, 1)); !errors.Is(err, wantErr) {
		t.Fatalf("publication error=%v", err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("lifecycle ok=%t err=%v", ok, err)
	}
	if lifecycle.State != core.GitHubPublicationRetrying || lifecycle.CreateState != core.GitHubCreateReconciling || lifecycle.Attempts != 1 || lifecycle.CreateAttempts != 1 || lifecycle.ForgeErrorCategory != string(githubtrigger.ForgeRequest) || lifecycle.LastError != wantErr.Error() {
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
		if payload.ForgeErrorCategory != string(githubtrigger.ForgeRequest) || payload.LastError != wantErr.Error() {
			t.Fatalf("retry category=%q last_error=%q", payload.ForgeErrorCategory, payload.LastError)
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

func TestReviewPublicationRequiresAggregateCommentAndIgnoresEventTimestamp(t *testing.T) {
	t.Parallel()
	ctx, st, worker, publication := reviewPublicationFixture(t, "approve")
	if err := st.AppendEvent(ctx, core.Event{
		TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.completed",
		At: publication.CreatedAt.Add(time.Second),
		Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": publication.ReviewWorkOrderID,
			"verdict":              "approve",
			"reason_code":          "approved",
			"summary":              "All criteria pass.",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker.dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		calls++
		if request.ReviewWorkOrderID != publication.ReviewWorkOrderID || len(request.History) != 1 ||
			request.History[0].WorkOrderID != publication.ReviewWorkOrderID {
			t.Fatalf("publication request=%+v", request)
		}
		return githubtrigger.ReviewPublicationResult{CommentID: 51, ReviewedCommitSHA: "head-1"}, nil
	}

	if err := worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 1)); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || stored.State != core.ReviewPublicationPublished || stored.CommentID != 51 || stored.ReviewedCommitSHA != "head-1" {
		t.Fatalf("stored publication=%+v err=%v", stored, err)
	}
	assertForgeWriteAuthor(t, st, ctx, publication.TaskID, "review.publication_published", core.ForgeAuthorHost)
	if err = worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 2)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("published retry called GitHub %d times, want 1", calls)
	}
}

func assertForgeWriteAuthor(t *testing.T, st store.Store, ctx context.Context, taskID, kind string, want core.ForgeAuthorClass) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != kind {
			continue
		}
		var payload struct {
			Class core.ForgeAuthorClass `json:"forge_author_class"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Class != want {
			t.Fatalf("%s forge author class = %q, want %q", kind, payload.Class, want)
		}
		return
	}
	t.Fatalf("event %s not found", kind)
}

func TestReviewPublicationFailureKeepsInternalReviewAuthoritative(t *testing.T) {
	t.Parallel()
	ctx, st, worker, publication := reviewPublicationFixture(t, "approve")
	wantErr := errors.New("GitHub comment publication failed")
	worker.dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{}, &githubtrigger.Error{Category: githubtrigger.ForgePermission, Err: wantErr}
	}
	if err := worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 1)); !errors.Is(err, wantErr) {
		t.Fatalf("publication error=%v, want %v", err, wantErr)
	}
	stored, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || stored.State != core.ReviewPublicationRetrying || stored.CommentID != 0 || stored.ForgeErrorCategory != string(githubtrigger.ForgePermission) || stored.LastError != wantErr.Error() {
		t.Fatalf("retrying publication=%+v err=%v", stored, err)
	}
	events, err := st.ListEvents(ctx, publication.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var failurePayload core.ReviewPublication
	for _, event := range events {
		if event.Kind == "review.publication_retry" {
			if err = json.Unmarshal(event.Payload, &failurePayload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if failurePayload.ForgeErrorCategory != string(githubtrigger.ForgePermission) || failurePayload.LastError != wantErr.Error() {
		t.Fatalf("review retry payload=%+v", failurePayload)
	}
	task, err := st.GetTask(ctx, publication.TaskID)
	if err != nil || task.State != core.TaskRunning || task.NextStage != core.StageReview {
		t.Fatalf("internal task changed after external failure: task=%+v err=%v", task, err)
	}

	worker.dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{ReviewedCommitSHA: "head-1"}, nil
	}
	err = worker.Work(ctx, reviewPublicationJob(publication.ReviewWorkOrderID, 5))
	if err == nil || !strings.Contains(err.Error(), "required aggregate comment returned no comment ID") {
		t.Fatalf("missing-comment error=%v", err)
	}
	stored, getErr := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if getErr != nil || stored.State != core.ReviewPublicationFailed || stored.CommentID != 0 {
		t.Fatalf("failed publication=%+v err=%v", stored, getErr)
	}
}

func reviewPublicationFixture(t *testing.T, verdict string) (context.Context, store.Store, *reviewPublicationWorker, core.ReviewPublication) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	taskID := "review-publication-" + core.NewTaskID()
	task := core.Task{
		ID: taskID, Workspace: "test", Repo: "app", Branch: "conveyor/" + taskID,
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobID := taskID + "-review-1"
	if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	publication := core.ReviewPublication{
		ReviewWorkOrderID: jobID, TaskID: taskID, JobID: jobID,
		Verdict: verdict, ReasonCode: "approved", Summary: "All criteria pass.",
	}
	if err := st.QueueReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	publication, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, &config.Config{
		Workspace: "test",
		Repos:     []config.Repo{{Name: "app", GitHub: "acme/app"}},
	}, nil)
	return ctx, st, &reviewPublicationWorker{dispatcher: dispatcher}, publication
}

func reviewPublicationJob(workOrderID string, attempt int) *river.Job[queueargs.ReviewPublicationArgs] {
	return &river.Job[queueargs.ReviewPublicationArgs]{
		JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt, MaxAttempts: 5},
		Args: queueargs.ReviewPublicationArgs{
			WorkspaceID: "test", ReviewWorkOrderID: workOrderID,
		},
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

func TestDispatchFinalRunningFailureParksWithFailFinal(t *testing.T) {
	t.Parallel()
	ctx, st, worker, taskID := dispatchFailureFixture(t, false)
	wantErr := errors.New("database unavailable")

	if err := worker.handleFailure(ctx, dispatchTaskJob(taskID, queueargs.DispatchTaskMaxAttempts, queueargs.DispatchTaskMaxAttempts), wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("failure error = %v, want %v", err, wantErr)
	}
	current, err := st.GetTask(ctx, taskID)
	if err != nil || current.State != core.TaskParked || current.RecoveryStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"dispatch.fail_final"`) {
			return
		}
	}
	t.Fatal("final dispatch failure did not record dispatch.fail_final")
}

func TestDispatchRetryDelayTableMatchesSpec(t *testing.T) {
	t.Parallel()
	if queueargs.DispatchTaskRetryLimit != 5 || queueargs.DispatchTaskMaxAttempts != 6 {
		t.Fatalf("retry limit/max attempts = %d/%d, want 5/6", queueargs.DispatchTaskRetryLimit, queueargs.DispatchTaskMaxAttempts)
	}
	wants := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second}
	for attempt, want := range wants {
		attempt := attempt + 1
		if got := queueargs.DispatchTaskRetryDelay(attempt); got != want {
			t.Fatalf("attempt %d retry delay = %s, want %s", attempt, got, want)
		}
	}
	if got := queueargs.DispatchTaskRetryDelay(6); got != queueargs.DispatchRetryMaximumDelay {
		t.Fatalf("retry cap = %s, want %s", got, queueargs.DispatchRetryMaximumDelay)
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
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict"}); err != nil {
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
