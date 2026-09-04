package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

func waitForJob(t *testing.T, ctx context.Context, st *storepg.Store, workspace string, stream eventlog.StreamID, matches func(logqueue.Job) bool, description string) logqueue.Job {
	t.Helper()
	for {
		job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
		if err != nil {
			t.Fatal(err)
		}
		if matches(job) {
			return job
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v (job=%+v)", description, ctx.Err(), job)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// TestQueueRuntimeConvergesLateWorkspacesIntegration: a workspace created
// after the runtime started is picked up by the registrar, and its queued
// tasks dispatch. A blueprint parent is a passive anchor whose child owns
// delivery: the child's job runs, the parent's job completes without work.
func TestQueueRuntimeConvergesLateWorkspacesIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspaceA := "queue-converge-a-" + suffix
	workspaceB := "queue-converge-b-" + suffix
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspaceA, "Queue convergence A "+suffix, dispatchRaceConfig(workspaceA)); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, dispatchRaceConfig(workspaceA), nil)
	runtime, err := testRuntime(t, st.Log(), dispatcher, &ShutdownMarker{}, []string{workspaceA}, map[string]*config.Config{workspaceA: dispatchRaceConfig(workspaceA)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if stopErr := runtime.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop queue runtime: %v", stopErr)
		}
	}()

	if _, err = st.CreateWorkspace(actorCtx, workspaceB, "Queue convergence B "+suffix, dispatchRaceConfig(workspaceB)); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspaceB)
	parent := core.Task{
		ID: "queue-converge-parent-" + suffix, Workspace: workspaceB, Repo: "repo", Title: "Convergence parent",
		BaseBranch: "main", Branch: "conveyor/queue-converge-parent-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	child := core.Task{
		ID: "queue-converge-child-" + suffix, Workspace: workspaceB, Repo: "repo", Title: "Convergence child",
		BaseBranch: "main", Branch: "conveyor/queue-converge-child-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, ParentTaskID: parent.ID,
		OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, parent); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(taskCtx, child); err != nil {
		t.Fatal(err)
	}
	registrar := NewWorkspaceQueueRegistrar([]string{workspaceA}, runtime.EnsureWorkspace, t.Logf)
	workspaces, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var convergence []core.Workspace
	for _, workspace := range workspaces {
		if workspace.ID == workspaceA || workspace.ID == workspaceB {
			convergence = append(convergence, workspace)
		}
	}
	if len(convergence) != 2 {
		t.Fatalf("convergence workspaces=%v", convergence)
	}
	if err = registrar.Converge(convergence); err != nil {
		t.Fatal(err)
	}
	kind := queueargs.DispatchTaskArgs{}.Kind()
	// The child's dispatch ran: an implement-stage task without a work
	// order is snoozed into its next stage, so the job is scheduled with
	// the attempt handed back, or completed.
	childJob := waitForJob(t, ctx, st, workspaceB, logqueue.StreamFor(kind, child.ID), func(job logqueue.Job) bool {
		return job.State == logqueue.StateCompleted || job.State == logqueue.StateScheduled
	}, "late workspace child dispatch")
	if childJob.Generation != 1 || childJob.ClaimedAt.IsZero() {
		t.Fatalf("child job=%+v, want one claimed generation", childJob)
	}
	parentJob, err := logqueue.Load(ctx, st.Log(), workspaceB, logqueue.StreamFor(kind, parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	if parentJob.State != logqueue.StateNone && parentJob.State != logqueue.StateCompleted {
		t.Fatalf("blueprint parent job=%+v, want none or completed without a snooze", parentJob)
	}
	current, err := st.GetTask(taskCtx, parent.ID)
	if err != nil || current.State != core.TaskQueued {
		t.Fatalf("blueprint parent task=%+v err=%v", current, err)
	}
}

// TestQueueDispatchCompletesBlueprintAnchorIntegration: dispatching a
// blueprint parent through the handler completes the job without a snooze
// and leaves the parent queued.
func TestQueueDispatchCompletesBlueprintAnchorIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx := t.Context()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "blueprint-anchor-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Blueprint anchor "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	parent := core.Task{
		ID: "blueprint-parent-" + suffix, Workspace: workspace, Repo: "repo", Title: "Blueprint parent",
		BaseBranch: "main", Branch: "conveyor/blueprint-parent-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	child := core.Task{
		ID: "blueprint-child-" + suffix, Workspace: workspace, Repo: "repo", Title: "Blueprint child",
		BaseBranch: "main", Branch: "conveyor/blueprint-child-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, ParentTaskID: parent.ID,
		OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, parent); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(taskCtx, child); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	worker := &dispatchTaskWorker{dispatcher: dispatcher}
	if err = worker.Work(ctx, testJob(queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: parent.ID}, 1, 1, queueargs.DispatchTaskMaxAttempts)); err != nil {
		t.Fatalf("blueprint dispatch returned %v, want completion", err)
	}
	persisted, err := st.GetTask(taskCtx, parent.ID)
	if err != nil || persisted.State != core.TaskQueued {
		t.Fatalf("blueprint parent=%+v err=%v", persisted, err)
	}
	orders, err := st.ListTaskWorkOrders(taskCtx, parent.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("blueprint parent orders=%+v err=%v, want none", orders, err)
	}
}

// TestQueueDispatchPersistsRetryDelaysThenParksIntegration: a dispatch that
// keeps failing records the spec's backoff on the log for its first retry,
// and after the final attempt the job is discarded and the task parked.
func TestQueueDispatchPersistsRetryDelaysThenParksIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "retry-policy-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Retry policy "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "retry-policy-" + suffix, Workspace: workspace, Repo: "repo", Title: "Retry policy",
		BaseBranch: "main", Branch: "conveyor/retry-policy-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	wantFailure := errors.New("forced dispatch failure")
	dispatcher := New(&failingStageOrderStore{Store: st, err: wantFailure}, cfg, nil)
	stream := logqueue.StreamFor(queueargs.DispatchTaskArgs{}.Kind(), task.ID)

	// Phase one: the real policy. The first failure schedules the retry ten
	// seconds out, which the log records without the test waiting for it.
	first, err := testRuntime(t, st.Log(), dispatcher, &ShutdownMarker{}, []string{workspace}, map[string]*config.Config{workspace: cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	failed := waitForJob(t, ctx, st, workspace, stream, func(job logqueue.Job) bool { return job.State == logqueue.StateScheduled && job.Attempt == 1 }, "first failure")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err = first.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stopCancel()
	if delay := failed.ScheduledAt.Sub(failed.ClaimedAt); delay < 10*time.Second || delay > 12*time.Second {
		t.Fatalf("first retry delay=%s, want %s", delay, queueargs.DispatchTaskRetryDelay(1))
	}
	if failed.LastError != wantFailure.Error() {
		t.Fatalf("recorded error=%q", failed.LastError)
	}

	// Phase two: the same job with a zero delay runs its remaining attempts
	// back to back, is discarded after the last, and the task is parked.
	if _, err := st.Log().Append(ctx, workspace, stream, failed.Head, []eventlog.NewEvent{{Kind: logqueue.KindRescued, Payload: []byte(`{"attempt":1,"next_at":"2000-01-01T00:00:00Z"}`)}}); err != nil {
		t.Fatal(err)
	}
	second, err := testRuntime(t, st.Log(), dispatcher, &ShutdownMarker{}, []string{workspace}, map[string]*config.Config{workspace: cfg}, func(registration *queueargs.Registration) {
		registration.RetryDelay = func(int) time.Duration { return 0 }
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = second.Stop(stopCtx)
	}()
	discarded := waitForJob(t, ctx, st, workspace, stream, func(job logqueue.Job) bool { return job.State == logqueue.StateDiscarded }, "discard after final attempt")
	if discarded.Attempt != queueargs.DispatchTaskMaxAttempts || discarded.MaxAttempts != queueargs.DispatchTaskMaxAttempts {
		t.Fatalf("discarded job=%+v", discarded)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		parked, err := st.GetTask(taskCtx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if parked.State == core.TaskParked && parked.RecoveryStage == core.StageImplement {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task after final attempt=%+v, want parked with implement recovery", parked)
		}
		time.Sleep(25 * time.Millisecond)
	}
	events, err := st.ListEvents(taskCtx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, event := range events {
		if event.Kind == "dispatch.failed" {
			failures++
		}
	}
	if failures != queueargs.DispatchTaskMaxAttempts {
		t.Fatalf("dispatch.failed events=%d, want %d", failures, queueargs.DispatchTaskMaxAttempts)
	}
}
