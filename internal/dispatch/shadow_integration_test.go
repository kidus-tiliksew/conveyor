package dispatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

// TestQueueShadowMirrorsRiverClaimsAndOutcomesIntegration runs River as the
// executor with the shadow attached: the store dual-enqueues, the River
// adapter reports the claim, and a hard stop's snooze lands on the log's
// job stream. The shadow's verdict for the one claim is agreement.
func TestQueueShadowMirrorsRiverClaimsAndOutcomesIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.EnableQueueShadow(t.Logf)

	suffix := core.NewTaskID()
	workspace := "queue-shadow-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Queue shadow "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	shadow := logqueue.NewShadow(st.Log(), logqueue.ShadowOptions{Workspaces: []string{workspace}, PollInterval: 50 * time.Millisecond, Logf: t.Logf})
	if err = shadow.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer shadow.Stop(context.Background())

	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "queue-shadow-" + suffix, Workspace: workspace, Repo: "repo", Title: "Shadowed dispatch",
		BaseBranch: "main", Branch: "conveyor/queue-shadow-" + suffix,
		State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	stream := logqueue.StreamFor(queueargs.DispatchTaskArgs{}.Kind(), task.ID)
	if job, err := logqueue.Load(ctx, st.Log(), workspace, stream); err != nil || job.State != logqueue.StateAvailable {
		t.Fatalf("dual-enqueued job=%+v err=%v", job, err)
	}

	blocking := &blockingDispatchStore{Store: st, taskID: task.ID, started: make(chan struct{})}
	dispatcher := New(blocking, cfg, nil)
	marker := &ShutdownMarker{}
	runtime, err := testRuntime(t, st.Pool(), dispatcher, marker, []string{workspace}, map[string]*config.Config{workspace: cfg}, shadow)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-ctx.Done():
		t.Fatal("River dispatch did not start")
	}
	if job, _ := logqueue.Load(ctx, st.Log(), workspace, stream); job.State != logqueue.StateRunning || job.Attempt != 1 || job.ClaimedBy != "river" {
		t.Fatalf("mirrored claim=%+v", job)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err = NewMarkedRuntime(runtime, marker).StopAndCancel(stopCtx); err != nil {
		t.Fatal(err)
	}
	job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || job.State != logqueue.StateScheduled || job.Attempt != 0 {
		t.Fatalf("job after interrupted River run=%+v err=%v, want the snooze mirrored", job, err)
	}
	events, _ := st.Log().Read(ctx, workspace, stream, 0, 0)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{logqueue.KindEnqueued, logqueue.KindClaimed, logqueue.KindSnoozed}) {
		t.Fatalf("job stream kinds=%v", kinds)
	}
	report := shadow.Report()
	if !report.Clean() || len(report.Counts) != 1 || report.Counts[0].Claims != 1 || report.Counts[0].Agree != 1 || report.Counts[0].Mirrored != 2 {
		t.Fatalf("shadow report=%+v", report)
	}
}
