package postgres

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// Pushed task streams and await_review on PostgreSQL (component-http-api,
// "When a task stream reads" and "Workspace change stream";
// component-work-orders, await_review; feature-quiet-idle-runtime, "Pushed
// task streams and `await_review`"; req-quiet-idle-runtime AC-1.2, AC-2.2,
// AC-2.3).
//
// A query tracer on the store's own pool counts every statement the daemon
// sends. The 60 seconds without a change are simulated: the only timers on
// these paths are the 25-second keep-alive, which the test drives twice per
// stream, and await_review's 10-minute wait; the real idle window covers
// several periods of the retired one-second poll.

const (
	pushedStreamsToken = "pushed-streams-token"
	pushedStreamsIdle  = 3 * time.Second
	pushedStreamsBound = 2 * time.Second
)

// keepaliveChannels hands each stream its own keep-alive channel, in the
// order the streams open.
type keepaliveChannels struct {
	mu       sync.Mutex
	channels []chan time.Time
}

func (k *keepaliveChannels) open() (<-chan time.Time, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	c := make(chan time.Time)
	k.channels = append(k.channels, c)
	return c, func() {}
}

func (k *keepaliveChannels) get(i int) chan time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.channels[i]
}

// sseFrames reads one server-sent-event response into frames, one string per
// blank-line-terminated block.
func sseFrames(t *testing.T, ctx context.Context, url string) <-chan string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+pushedStreamsToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, response.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer response.Body.Close()
		defer close(frames)
		reader := bufio.NewReader(response.Body)
		var frame strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				frames <- frame.String()
				frame.Reset()
				continue
			}
			frame.WriteString(line)
		}
	}()
	return frames
}

func nextFrame(t *testing.T, frames <-chan string, what string) string {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatalf("%s: stream ended", what)
		}
		return frame
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no frame", what)
		return ""
	}
}

// awaitSignalStore signals each completed await_review read.
type awaitSignalStore struct {
	store.Store
	reads chan struct{}
}

func (s *awaitSignalStore) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	events, err := s.Store.ListCompactEvents(ctx, taskID)
	s.reads <- struct{}{}
	return events, err
}

func TestPushedTaskStreamsAndAwaitReviewIntegration(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(serverTimingSchemaURL(t))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "pushed-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), workspace)
	if _, err = st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Pushed", Email: "owner@example.test", DisplayName: "Owner"}, pushedStreamsToken); err != nil {
		t.Fatal(err)
	}
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Mode: core.TaskModeAuto, PolicyVersion: 1, Level: core.L2, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "pushed-session", ClientToken: "pushed-token", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 1})}); err != nil {
		t.Fatal(err)
	}

	keepalives := &keepaliveChannels{}
	server := httpapi.NewServer(st)
	server.Workspaces = st
	server.Workspace = workspace
	server.StreamKeepalive = keepalives.open
	if server.Changes != st.Changes() {
		t.Fatal("NewServer did not take the store's change hub")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	streamCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()

	// Open each stream and wait for the frame its first read produces.
	notify := sseFrames(t, streamCtx, httpServer.URL+"/v1/tasks/"+task.ID+"/events/stream?mode=notify&workspace_id="+workspace)
	if frame := nextFrame(t, notify, "notify ready"); frame != "event: ready\ndata: {}\n" {
		t.Fatalf("notify first frame=%q", frame)
	}
	legacy := sseFrames(t, streamCtx, httpServer.URL+"/v1/tasks/"+task.ID+"/events/stream?workspace_id="+workspace)
	if frame := nextFrame(t, legacy, "legacy replay"); !strings.HasPrefix(frame, "event: activity\ndata: {\"id\":") {
		t.Fatalf("legacy first frame=%q", frame)
	}
	// Drain the rest of the legacy replay, then confirm it is complete.
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range len(events) - 1 {
		nextFrame(t, legacy, "legacy replay")
	}
	workspaceStream := sseFrames(t, streamCtx, httpServer.URL+"/v1/workspaces/"+workspace+"/changes")
	if frame := nextFrame(t, workspaceStream, "workspace ready"); frame != "event: ready\ndata: {}\n" {
		t.Fatalf("workspace first frame=%q", frame)
	}
	awaitStore := &awaitSignalStore{Store: st, reads: make(chan struct{}, 8)}
	service := &workorder.Service{Store: awaitStore, Changes: st.Changes()}
	type awaitOutcome struct {
		result map[string]any
		err    error
	}
	awaitDone := make(chan awaitOutcome, 1)
	go func() {
		result, err := service.AwaitReview(ctx, order.ID, "pushed-session", 10*time.Minute)
		awaitDone <- awaitOutcome{result, err}
	}()
	select {
	case <-awaitStore.reads:
	case <-time.After(10 * time.Second):
		t.Fatal("await_review made no first read")
	}

	// Idle: no statement after the first reads, through the real window and
	// two keep-alives per stream (25 and 50 simulated seconds).
	before := len(recorder.snapshot())
	time.Sleep(pushedStreamsIdle)
	for _, stream := range []struct {
		index  int
		frames <-chan string
		name   string
	}{{0, notify, "notify"}, {1, legacy, "legacy"}, {2, workspaceStream, "workspace"}} {
		for range 2 {
			keepalives.get(stream.index) <- time.Time{}
			if frame := nextFrame(t, stream.frames, stream.name+" keep-alive"); frame != ": keepalive\n" {
				t.Fatalf("%s keep-alive frame=%q", stream.name, frame)
			}
		}
	}
	if idle := recorder.snapshot()[before:]; len(idle) != 0 {
		t.Fatalf("open streams and a pending await_review sent %d statements while idle: %q", len(idle), idle)
	}

	// A task event reaches every open stream within two seconds.
	committed := time.Now()
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.requested"}); err != nil {
		t.Fatal(err)
	}
	committedAt := time.Now()
	if frame := nextFrame(t, notify, "notify activity"); frame != "event: activity\ndata: {}\n" {
		t.Fatalf("notify activity=%q", frame)
	}
	if elapsed := time.Since(committedAt); elapsed > pushedStreamsBound {
		t.Fatalf("notify stream received the event %s after commit", elapsed)
	}
	if frame := nextFrame(t, legacy, "legacy activity"); !strings.Contains(frame, `"kind":"merge.requested"`) {
		t.Fatalf("legacy activity=%q", frame)
	}
	if elapsed := time.Since(committedAt); elapsed > pushedStreamsBound {
		t.Fatalf("legacy stream received the event %s after commit", elapsed)
	}
	if frame := nextFrame(t, workspaceStream, "workspace activity"); frame != "event: activity\ndata: {\"task_id\":\""+task.ID+"\"}\n" {
		t.Fatalf("workspace activity=%q", frame)
	}
	if elapsed := time.Since(committedAt); elapsed > pushedStreamsBound {
		t.Fatalf("workspace stream received the event %s after commit", elapsed)
	}
	t.Logf("task event delivered to three streams %s after the append began", time.Since(committed))
	// The unsettled event woke await_review once; it is still pending.
	select {
	case <-awaitStore.reads:
	case <-time.After(10 * time.Second):
		t.Fatal("await_review did not read on the task notice")
	}
	select {
	case outcome := <-awaitDone:
		t.Fatalf("await_review returned before the round settled: %v %v", outcome.result, outcome.err)
	default:
	}

	// await_review returns within two seconds of the round settling.
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approved", "review_round": 1})}); err != nil {
		t.Fatal(err)
	}
	settledAt := time.Now()
	select {
	case outcome := <-awaitDone:
		if elapsed := time.Since(settledAt); elapsed > pushedStreamsBound {
			t.Fatalf("await_review returned %s after the round settled", elapsed)
		}
		if outcome.err != nil || outcome.result["verdict"] != "approved" {
			t.Fatalf("await_review result=%v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("await_review did not return after the round settled")
	}
}
