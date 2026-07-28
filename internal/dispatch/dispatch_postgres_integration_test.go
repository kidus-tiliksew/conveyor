package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type observedTaskLockStore struct {
	store.Store
	attempts      atomic.Int32
	secondAttempt chan struct{}
	orderCreated  chan struct{}
	releaseOrder  chan struct{}
}

type failingTaskLockStore struct {
	store.Store
	err error
}

func (s *failingTaskLockStore) WithTaskLock(context.Context, string, func() error) error {
	return s.err
}

func TestRiverDispatchPersistsFiveRetriesThenParksIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "dispatch-retry-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Dispatch retry "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "dispatch-retry-" + suffix, Workspace: workspace, Repo: "repo", Title: "Retry dispatch",
		BaseBranch: "main", Branch: "conveyor/dispatch-retry-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.EnsureTaskEnqueued(taskCtx, task.ID); err != nil {
		t.Fatal(err)
	}

	wantFailure := errors.New("forced dispatch failure")
	dispatcher := New(&failingTaskLockStore{Store: st, err: wantFailure}, cfg, nil)
	client, err := NewRiverClient(st.Pool(), dispatcher, []string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if stopErr := client.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop River client: %v", stopErr)
		}
	}()

	var riverJobID int64
	if err = st.Pool().QueryRow(ctx, `SELECT id FROM river_job WHERE kind=$1 AND args->>'task_id'=$2`,
		(queueargs.DispatchTaskArgs{}).Kind(), task.ID).Scan(&riverJobID); err != nil {
		t.Fatal(err)
	}
	wants := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second}
	for index, want := range wants {
		attempt := index + 1
		row := waitForRiverDispatchState(t, ctx, client, riverJobID, attempt, rivertype.JobStateRetryable)
		if row.MaxAttempts != queueargs.DispatchTaskMaxAttempts || row.AttemptedAt == nil {
			t.Fatalf("attempt %d row=%+v", attempt, row)
		}
		if got := row.ScheduledAt.Sub(*row.AttemptedAt); got < want || got > want+2*time.Second {
			t.Fatalf("attempt %d persisted retry delay=%s, want %s", attempt, got, want)
		}
		if _, err = client.JobRetry(ctx, riverJobID); err != nil {
			t.Fatalf("release attempt %d retry: %v", attempt, err)
		}
	}

	final := waitForRiverDispatchState(t, ctx, client, riverJobID, queueargs.DispatchTaskMaxAttempts, rivertype.JobStateDiscarded)
	if final.MaxAttempts != queueargs.DispatchTaskMaxAttempts || len(final.Errors) != queueargs.DispatchTaskMaxAttempts {
		t.Fatalf("final River row=%+v", final)
	}
	parked, err := st.GetTask(taskCtx, task.ID)
	if err != nil || parked.State != core.TaskParked || parked.RecoveryStage != core.StageImplement {
		t.Fatalf("parked task=%+v err=%v", parked, err)
	}
	events, err := st.ListEvents(taskCtx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		var payload struct {
			Command core.TaskCommand `json:"command"`
		}
		if event.Kind == "task.state_changed" && json.Unmarshal(event.Payload, &payload) == nil && payload.Command == core.TaskDispatchFailFinal {
			return
		}
	}
	t.Fatal("final River execution did not persist dispatch.fail_final")
}

func waitForRiverDispatchState(t *testing.T, ctx context.Context, client *river.Client[pgx.Tx], jobID int64, attempt int, state rivertype.JobState) *rivertype.JobRow {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		row, err := client.JobGet(ctx, jobID)
		if err == nil && row.Attempt == attempt && row.State == state {
			return row
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for River job %d attempt %d state %s: %v (last row=%+v err=%v)", jobID, attempt, state, ctx.Err(), row, err)
		case <-ticker.C:
		}
	}
}

func (s *observedTaskLockStore) WithTaskLock(ctx context.Context, taskID string, fn func() error) error {
	if s.attempts.Add(1) == 2 {
		close(s.secondAttempt)
	}
	return s.Store.WithTaskLock(ctx, taskID, fn)
}

func (s *observedTaskLockStore) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	if err := s.Store.CreateWorkOrder(ctx, order); err != nil {
		return err
	}
	if order.Stage == core.StageImplement && order.ReasonCode == "merge-conflict" {
		close(s.orderCreated)
		<-s.releaseOrder
	}
	return nil
}

func TestConflictFixAndRiverDispatchSerializeIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := t.Context()
	st, err := storepg.Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "dispatch-race-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Dispatch race "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	task := core.Task{
		ID: "conflict-race-" + suffix, Workspace: workspace, Repo: "repo", Title: "Resolve conflict",
		BaseBranch: "main", Branch: "conveyor/conflict-race-" + suffix,
		State: core.TaskApproved, CreatedAt: time.Now(),
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}

	observed := &observedTaskLockStore{
		Store: st, secondAttempt: make(chan struct{}),
		orderCreated: make(chan struct{}), releaseOrder: make(chan struct{}),
	}
	releasedOrder := false
	defer func() {
		if !releasedOrder {
			close(observed.releaseOrder)
		}
	}()
	dispatcher := New(observed, cfg, nil)
	dispatcher.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 7, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}

	conflictDone := make(chan error, 1)
	go func() {
		_, dispatchErr := dispatcher.DispatchConflictFix(ctx, task)
		conflictDone <- dispatchErr
	}()
	select {
	case <-observed.orderCreated:
	case <-time.After(5 * time.Second):
		t.Fatal("conflict dispatch did not create the work order")
	}

	riverDone := make(chan error, 1)
	worker := &dispatchTaskWorker{dispatcher: dispatcher}
	go func() {
		riverDone <- worker.Work(ctx, &river.Job[queueargs.DispatchTaskArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 5},
			Args:   queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: task.ID},
		})
	}()
	select {
	case <-observed.secondAttempt:
	case <-time.After(5 * time.Second):
		t.Fatal("River dispatch did not reach the shared task lock")
	}
	close(observed.releaseOrder)
	releasedOrder = true
	select {
	case err = <-conflictDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conflict dispatch did not finish")
	}
	select {
	case err = <-riverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("River dispatch did not finish")
	}

	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" || orders[0].BaselineSHA != "approved-head" {
		t.Fatalf("conflict-fix orders = %+v", orders)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed events = %d, err = %v", count, countErr)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "pipeline.transition_decided"); countErr != nil || count != 1 {
		t.Fatalf("pipeline.transition_decided events = %d, err = %v", count, countErr)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskRunning || current.NextStage != core.StageImplement {
		t.Fatalf("task after overlap = %+v, err = %v", current, err)
	}

	claims := make(chan error, 2)
	for i := range 2 {
		go func() {
			_, claimErr := st.ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{
				SessionID: "session-" + string(rune('a'+i)), ClientToken: "token-" + string(rune('a'+i)),
				Agent: "codex", Model: "operator", Lease: time.Minute, ExecutionTimeout: time.Hour,
			})
			claims <- claimErr
		}()
	}
	succeeded := 0
	for range 2 {
		if claimErr := <-claims; claimErr == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent claims = %d, want 1", succeeded)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.claimed"); countErr != nil || count != 1 {
		t.Fatalf("work_order.claimed events = %d, err = %v", count, countErr)
	}
}

func dispatchIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse CONVEYOR_TEST_DATABASE_URL: %v", err)
	}
	if !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("refusing integration database %q: name must end in _test", parsed.Path)
	}
	return databaseURL
}

func dispatchRaceConfig(workspace string) *config.Config {
	return &config.Config{
		Workspace: workspace,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo.git", GitHub: "acme/repo", Base: "main"}},
	}
}
