// Package publicationfixture drives the AR-5 automatic publication re-enqueue
// through the durable queue on a real backend: the sweep enqueues one job per
// failed publication, and the production publication handlers, run by the
// log-backed queue runtime, take each row from retrying to published
// (feature-bounded-automatic-recovery AC-5.1).
package publicationfixture

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Run seeds one task whose GitHub issue lifecycle and review publication have
// both failed with their queue jobs settled, runs the automatic sweep, then
// runs the production handlers on the durable queue until both rows publish.
func Run(t *testing.T, open func(*testing.T) store.Backend) {
	t.Helper()
	st := open(t)
	if !st.IsDurable() {
		t.Fatal("publication fixture requires a durable backend")
	}
	root := store.WithActor(t.Context(), store.SystemActor())
	workspace := "auto-publication-" + core.NewTaskID()
	// Issue publication is on, so the re-enqueued issue row publishes rather
	// than ending skipped (req-delivery-and-forge AC-8.4).
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		IssuePublication: config.IssuePublication{Enabled: true}}
	if _, err := st.CreateWorkspace(store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman}), workspace, workspace, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	taskID, reviewID := seedFailedPublications(t, st, ctx, workspace)

	now := time.Now().UTC()
	if acted, err := store.ReconcileFailedPublications(ctx, st, now); err != nil || acted != 2 {
		t.Fatalf("automatic sweep acted=%d err=%v, want both failed rows re-enqueued", acted, err)
	}
	issueStream := logqueue.StreamFor(queue.GitHubIssuePublicationArgs{}.Kind(), taskID)
	reviewStream := logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), reviewID)
	for _, stream := range []eventlog.StreamID{issueStream, reviewStream} {
		job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
		if err != nil || !job.Active() {
			t.Fatalf("sweep left no active job on %s: %+v err=%v", stream, job, err)
		}
	}

	var mu sync.Mutex
	var issues, reviews int
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.PublishIssue = func(context.Context, githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		mu.Lock()
		defer mu.Unlock()
		issues++
		return githubtrigger.IssuePublicationResult{Number: 41, URL: "https://github.com/acme/app/issues/41"}, nil
	}
	dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		mu.Lock()
		defer mu.Unlock()
		reviews++
		if request.Verdict != "changes_requested" || request.Summary != "keep this summary" {
			t.Errorf("re-enqueued review published changed verdict fields: %+v", request)
		}
		return githubtrigger.ReviewPublicationResult{CommentID: 71, ReviewedCommitSHA: "head-1"}, nil
	}
	runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
		Workspaces: []string{workspace}, PollInterval: 20 * time.Millisecond, RescueStuckAfter: time.Minute, WorkerID: "publication-fixture",
	})
	for _, registration := range dispatcher.Registrations(nil) {
		if registration.Kind == (queue.GitHubIssuePublicationArgs{}).Kind() || registration.Kind == (queue.ReviewPublicationArgs{}).Kind() {
			runtime.Register(registration)
		}
	}
	if err := runtime.Start(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runtime.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		publication, err := st.GetReviewPublication(ctx, reviewID)
		if err != nil {
			t.Fatal(err)
		}
		if lifecycle.State == core.GitHubPublicationPublished && publication.State == core.ReviewPublicationPublished {
			if lifecycle.IssueNumber != 41 || publication.CommentID != 71 || publication.ReviewedCommitSHA != "head-1" {
				t.Fatalf("published lifecycle=%+v publication=%+v", lifecycle, publication)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("re-enqueued rows never published: lifecycle=%s (%s) publication=%s (%s)", lifecycle.State, lifecycle.LastError, publication.State, publication.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if issues != 1 || reviews != 1 {
		t.Fatalf("forge calls issues=%d reviews=%d, want one each", issues, reviews)
	}
}

// seedFailedPublications queues both publication rows for a running task
// with an approved plan, settles their queue jobs as a worker's last attempt
// would, and marks both rows failed.
func seedFailedPublications(t *testing.T, st store.Backend, ctx context.Context, workspace string) (string, string) {
	t.Helper()
	taskID := "auto-publication-" + core.NewTaskID()
	reviewID := taskID + "-review"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "app", Title: "Automatic publication recovery", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}))
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
	must(err)
	must(st.ApproveSpecVersion(ctx, taskID, spec.Version))
	must(st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: taskID, Repository: "acme/app", SpecVersion: spec.Version}))
	must(st.CreateJob(ctx, core.Job{ID: reviewID, TaskID: taskID, Stage: core.StageReview, State: core.JobDone}))
	must(st.QueueReviewPublication(ctx, core.ReviewPublication{ReviewWorkOrderID: reviewID, TaskID: taskID, JobID: reviewID, Verdict: "changes_requested", Summary: "keep this summary", ReviewedCommitSHA: "head-1"}))
	for _, stream := range []eventlog.StreamID{
		logqueue.StreamFor(queue.GitHubIssuePublicationArgs{}.Kind(), taskID),
		logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), reviewID),
	} {
		job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
		must(err)
		if job.Active() {
			_, err = st.Log().Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{Kind: logqueue.KindCompleted, Payload: []byte(`{}`), At: time.Now().UTC()}})
			must(err)
		}
	}
	lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
	must(err)
	lifecycle.State, lifecycle.LastError, lifecycle.ForgeErrorCategory = core.GitHubPublicationFailed, "issue publish failed", "forge_request"
	must(st.UpdateGitHubLifecycle(ctx, lifecycle))
	publication, err := st.GetReviewPublication(ctx, reviewID)
	must(err)
	publication.State, publication.LastError, publication.ForgeErrorCategory = core.ReviewPublicationFailed, "review publish failed", "forge_request"
	must(st.UpdateReviewPublication(ctx, publication))
	return taskID, reviewID
}
