package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

// TestLogQueueShutdownInterruptionPreservesAttemptIntegration: the
// dispatcher's handler runs from a job on the log, a hard stop interrupts
// it, and the job is left scheduled with its attempt handed back rather
// than failed.
func TestLogQueueShutdownInterruptionPreservesAttemptIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "logqueue-interrupt-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Log queue interrupt "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "logqueue-interrupt-" + suffix, Workspace: workspace, Repo: "repo", Title: "Interrupt dispatch",
		BaseBranch: "main", Branch: "conveyor/logqueue-interrupt-" + suffix,
		State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	stream := logqueue.StreamFor(queueargs.DispatchTaskArgs{}.Kind(), task.ID)
	before, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || before.State != logqueue.StateAvailable {
		t.Fatalf("job before dispatch=%+v err=%v", before, err)
	}

	blocking := &blockingDispatchStore{Store: st, taskID: task.ID, started: make(chan struct{})}
	dispatcher := New(blocking, cfg, nil)
	marker := &ShutdownMarker{}
	runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
		Workspaces: []string{workspace}, PollInterval: 50 * time.Millisecond, ClockInterval: -1,
		RescueStuckAfter: time.Hour, WorkerID: "test-" + suffix, Logf: t.Logf,
	})
	for _, registration := range dispatcher.Registrations(marker) {
		runtime.Register(registration)
	}
	if err = runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-ctx.Done():
		t.Fatal("log queue dispatch did not start")
	}
	running, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || running.State != logqueue.StateRunning || running.Attempt != 1 {
		t.Fatalf("job while running=%+v err=%v", running, err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err = NewMarkedRuntime(runtime, marker).StopAndCancel(stopCtx); err != nil {
		t.Fatal(err)
	}
	after, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != logqueue.StateScheduled || after.Attempt != 0 || after.Generation != 1 {
		t.Fatalf("job after interruption=%+v, want scheduled with attempt 0 on the same generation", after)
	}
	if count, countErr := st.CountEvents(taskCtx, task.ID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed events=%d err=%v, want 0", count, countErr)
	}
	current, err := st.GetTask(taskCtx, task.ID)
	if err != nil || current.State != core.TaskQueued {
		t.Fatalf("task=%+v err=%v, want no failure transition", current, err)
	}
}
