package storetest

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunAutomaticPublicationRecoveryConformance proves the AR-5 publication
// rule (feature-bounded-automatic-recovery AC-5.1, AC-5.4, AC-6.1) on every
// backend: one automatic re-enqueue per failed publication per recovery
// window, verdict fields copied unchanged, audit marked automatic, and no
// action at bound 0 or on a task that is not queued or running.
func RunAutomaticPublicationRecoveryConformance(t *testing.T, st store.Backend, ctx context.Context, workspace string, sweep ReviewSweepFunc) {
	t.Run("OncePerWindow", func(t *testing.T) {
		restore := setAutomaticRecoveryBound(t, st, ctx, 2)
		defer restore()
		window := automaticRecoveryWindow(t, st, ctx)
		taskID, reviewID := seedFailedPublications(t, st, ctx, workspace, core.TaskRunning)
		now := time.Now().UTC().Truncate(time.Second)
		// Startup reconciliation re-enqueues only queued or retrying rows; a
		// failed row belongs to the bounded automatic rule alone (D8).
		if n, err := st.ReconcileGitHubLifecycles(ctx); err != nil || n != 0 {
			t.Fatalf("startup reconcile of a failed lifecycle=%d err=%v", n, err)
		}
		if n, err := st.ReconcileReviewPublications(ctx); err != nil || n != 0 {
			t.Fatalf("startup reconcile of a failed review publication=%d err=%v", n, err)
		}

		n, err := store.ReconcileFailedPublications(ctx, st, now)
		if err != nil || n != 2 {
			t.Fatalf("first sweep acted=%d err=%v", n, err)
		}
		requirePublicationJobs(t, st, ctx, workspace, taskID, reviewID)
		lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
		requireOK(t, err)
		publication, err := st.GetReviewPublication(ctx, reviewID)
		requireOK(t, err)
		if lifecycle.State != core.GitHubPublicationRetrying || !lifecycle.AutomaticReenqueueAt.Equal(now) {
			t.Fatalf("lifecycle after automatic retry=%+v", lifecycle)
		}
		if publication.State != core.ReviewPublicationRetrying || !publication.AutomaticReenqueueAt.Equal(now) ||
			publication.Verdict != "changes_requested" || publication.Feedback != "keep this verdict" || publication.Summary != "keep this summary" ||
			publication.ReviewedCommitSHA != automaticPublicationReviewedSHA {
			t.Fatalf("publication after automatic retry=%+v", publication)
		}
		assertAutomaticPublicationEvents(t, st, ctx, taskID, 1)

		if n, err = store.ReconcileFailedPublications(ctx, st, now); err != nil || n != 0 {
			t.Fatalf("repeat sweep acted=%d err=%v", n, err)
		}
		workspaceID, _ := store.WorkspaceFromContext(ctx)
		completePublicationJobs(t, st, ctx, workspaceID, taskID, reviewID)
		failPublications(t, st, ctx, taskID, reviewID)
		if n, err = store.ReconcileFailedPublications(ctx, st, now.Add(window-time.Second)); err != nil || n != 0 {
			t.Fatalf("in-window sweep acted=%d err=%v", n, err)
		}
		if retries, err := st.ListAutomaticPublicationRetries(ctx, now.Add(window-time.Second)); err != nil || len(retries) != 0 {
			t.Fatalf("in-window retries=%+v err=%v", retries, err)
		}
		forge := store.LatestForgeFailure(listEventsOK(t, st, ctx, taskID))
		if forge == nil || !forge.AutomaticRetryExhausted {
			t.Fatalf("forge failure after automatic retry=%+v", forge)
		}
		assertAutomaticPublicationEvents(t, st, ctx, taskID, 1)

		next := now.Add(window + time.Second)
		if n, err = store.ReconcileFailedPublications(ctx, st, next); err != nil || n != 2 {
			t.Fatalf("next-window sweep acted=%d err=%v", n, err)
		}
		assertAutomaticPublicationEvents(t, st, ctx, taskID, 2)
	})
	t.Run("ConcurrentSweepsActOnce", func(t *testing.T) {
		restore := setAutomaticRecoveryBound(t, st, ctx, 2)
		defer restore()
		taskID, _ := seedFailedPublications(t, st, ctx, workspace, core.TaskRunning)
		now := time.Now().UTC().Truncate(time.Second)
		var wg sync.WaitGroup
		var acted atomic.Int64
		errs := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := store.ReconcileFailedPublications(ctx, st, now)
				acted.Add(int64(n))
				if err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Logf("concurrent sweep error: %v", err)
		}
		// Only this task's two rows are due; a refused act is not counted.
		if acted.Load() != 2 {
			t.Fatalf("concurrent sweeps reported %d acts", acted.Load())
		}
		assertAutomaticPublicationEvents(t, st, ctx, taskID, 1)
	})
	t.Run("NoAction", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			bound int
			state core.TaskState
		}{
			{"BoundZero", 0, core.TaskRunning},
			{"Terminal", 2, core.TaskMerged},
			{"Awaiting", 2, core.TaskAwaiting},
			{"Parked", 2, core.TaskParked},
		} {
			t.Run(tc.name, func(t *testing.T) {
				restore := setAutomaticRecoveryBound(t, st, ctx, tc.bound)
				defer restore()
				taskID, reviewID := seedFailedPublications(t, st, ctx, workspace, tc.state)
				// Rows of earlier subtests may act here; only this task's rows count.
				if _, err := store.ReconcileFailedPublications(ctx, st, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
				requireOK(t, err)
				publication, err := st.GetReviewPublication(ctx, reviewID)
				requireOK(t, err)
				if lifecycle.State != core.GitHubPublicationFailed || publication.State != core.ReviewPublicationFailed {
					t.Fatalf("lifecycle=%s publication=%s", lifecycle.State, publication.State)
				}
				assertAutomaticPublicationEvents(t, st, ctx, taskID, 0)
			})
		}
	})
	if st.IsDurable() {
		t.Run("ActiveJobLeavesRowFailed", func(t *testing.T) {
			restore := setAutomaticRecoveryBound(t, st, ctx, 2)
			defer restore()
			taskID, reviewID := seedFailedPublicationsWithJobs(t, st, ctx, workspace, core.TaskRunning, false)
			if n, err := store.ReconcileFailedPublications(ctx, st, time.Now().UTC()); err != nil || n != 0 {
				t.Fatalf("sweep over rows with active jobs acted=%d err=%v", n, err)
			}
			lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
			requireOK(t, err)
			publication, err := st.GetReviewPublication(ctx, reviewID)
			requireOK(t, err)
			if lifecycle.State != core.GitHubPublicationFailed || publication.State != core.ReviewPublicationFailed {
				t.Fatalf("lifecycle=%s publication=%s", lifecycle.State, publication.State)
			}
			assertAutomaticPublicationEvents(t, st, ctx, taskID, 0)
		})
	}
	t.Run("NothingDueChangesNothing", func(t *testing.T) {
		runNothingDueSweeps(t, st, ctx, sweep)
	})
}

func automaticRecoveryWindow(t *testing.T, st store.Store, ctx context.Context) time.Duration {
	t.Helper()
	reader, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := reader.WorkspaceConfig(ctx)
	requireOK(t, err)
	window := current.Document.Recovery.Window
	if window <= 0 {
		parsed, err := time.ParseDuration(current.Document.Recovery.WindowText)
		if err != nil || parsed <= 0 {
			t.Fatalf("recovery window=%q err=%v", current.Document.Recovery.WindowText, err)
		}
		window = parsed
	}
	return window
}

func seedFailedPublications(t *testing.T, st store.Backend, ctx context.Context, workspace string, state core.TaskState) (string, string) {
	t.Helper()
	return seedFailedPublicationsWithJobs(t, st, ctx, workspace, state, true)
}

// seedFailedPublicationsWithJobs creates one failed GitHub-issue lifecycle
// and one failed review publication. Durable backends enqueue a publication
// job with each row; completeJobs settles those jobs, as a worker would.
func seedFailedPublicationsWithJobs(t *testing.T, st store.Backend, ctx context.Context, workspace string, state core.TaskState, completeJobs bool) (string, string) {
	t.Helper()
	taskID := "auto-publication-" + core.NewTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "app", Title: "Automatic publication recovery", BaseBranch: "main", Branch: "conveyor/" + taskID, State: state, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}))
	requireOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: taskID, Repository: "acme/app", SpecVersion: 1}))
	reviewID := taskID + "-review"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: reviewID, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}))
	requireOK(t, st.QueueReviewPublication(ctx, core.ReviewPublication{ReviewWorkOrderID: reviewID, TaskID: taskID, JobID: reviewID, Verdict: "changes_requested", Feedback: "keep this verdict", Summary: "keep this summary", ReviewedCommitSHA: automaticPublicationReviewedSHA}))
	if completeJobs {
		completePublicationJobs(t, st, ctx, workspace, taskID, reviewID)
	}
	failPublications(t, st, ctx, taskID, reviewID)
	return taskID, reviewID
}

// failPublications moves both rows to failed, as a worker's last attempt does.
func failPublications(t *testing.T, st store.Store, ctx context.Context, taskID, reviewID string) {
	t.Helper()
	lifecycle, _, err := st.GetGitHubLifecycle(ctx, taskID)
	requireOK(t, err)
	lifecycle.State, lifecycle.LastError, lifecycle.ForgeErrorCategory = core.GitHubPublicationFailed, "issue publish failed", "forge_request"
	requireOK(t, st.UpdateGitHubLifecycle(ctx, lifecycle))
	publication, err := st.GetReviewPublication(ctx, reviewID)
	requireOK(t, err)
	publication.State, publication.LastError, publication.ForgeErrorCategory = core.ReviewPublicationFailed, "review publish failed", "forge_request"
	requireOK(t, st.UpdateReviewPublication(ctx, publication))
}

func completePublicationJobs(t *testing.T, st store.Backend, ctx context.Context, workspace, taskID, reviewID string) {
	t.Helper()
	if !st.IsDurable() {
		return
	}
	for _, stream := range []eventlog.StreamID{
		logqueue.StreamFor(queue.GitHubIssuePublicationArgs{}.Kind(), taskID),
		logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), reviewID),
	} {
		job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
		requireOK(t, err)
		if !job.Active() {
			continue
		}
		_, err = st.Log().Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{Kind: logqueue.KindCompleted, Payload: []byte(`{}`), At: time.Now().UTC()}})
		requireOK(t, err)
	}
}

// requirePublicationJobs requires an active publication job on both streams
// of a durable backend: the automatic re-enqueue reached the worker queue.
func requirePublicationJobs(t *testing.T, st store.Backend, ctx context.Context, workspace, taskID, reviewID string) {
	t.Helper()
	if !st.IsDurable() {
		return
	}
	for _, stream := range []eventlog.StreamID{
		logqueue.StreamFor(queue.GitHubIssuePublicationArgs{}.Kind(), taskID),
		logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), reviewID),
	} {
		job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
		if err != nil || !job.Active() {
			t.Fatalf("publication job on %s=%+v err=%v", stream, job, err)
		}
	}
}

// automaticPublicationReviewedSHA is the reviewed head a re-enqueue must keep.
const automaticPublicationReviewedSHA = "0123456789abcdef0123456789abcdef01234567"

func listEventsOK(t *testing.T, st store.Store, ctx context.Context, taskID string) []core.Event {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	return events
}

// assertAutomaticPublicationEvents requires exactly want automatic retry
// events per publication kind, each attributed to the automatic actor with
// a distinct request ID and opening its window at the re-enqueue stamp.
func assertAutomaticPublicationEvents(t *testing.T, st store.Store, ctx context.Context, taskID string, want int) {
	t.Helper()
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, event := range listEventsOK(t, st, ctx, taskID) {
		if event.Kind != "github_issue.publication_retry" && event.Kind != "review.publication_retry" {
			continue
		}
		var payload struct {
			Automatic            bool      `json:"automatic"`
			RequestID            string    `json:"request_id"`
			WindowStartedAt      time.Time `json:"window_started_at"`
			AutomaticReenqueueAt time.Time `json:"automatic_reenqueue_at"`
		}
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if !payload.Automatic {
			continue
		}
		if event.ActorID != store.AutomaticPublicationActor(taskID).ID || payload.RequestID == "" || seen[payload.RequestID] {
			t.Fatalf("automatic retry event actor=%s request=%q duplicate=%t", event.ActorID, payload.RequestID, seen[payload.RequestID])
		}
		if payload.WindowStartedAt.IsZero() || !payload.WindowStartedAt.Equal(payload.AutomaticReenqueueAt) {
			t.Fatalf("automatic retry event window_started_at=%v stamp=%v", payload.WindowStartedAt, payload.AutomaticReenqueueAt)
		}
		seen[payload.RequestID] = true
		counts[event.Kind]++
	}
	if counts["github_issue.publication_retry"] != want || counts["review.publication_retry"] != want {
		t.Fatalf("automatic retry events=%v want %d each", counts, want)
	}
}
