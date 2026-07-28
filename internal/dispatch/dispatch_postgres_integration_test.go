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

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
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

	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	wantFailure := errors.New("forced dispatch failure")
	dispatcher := New(&failingTaskLockStore{Store: st, err: wantFailure}, cfg, nil)
	testWorker := rivertest.NewWorker(t, riverpgxv5.New(nil), &river.Config{ID: "dispatch-retry-policy"}, &dispatchTaskWorker{dispatcher: dispatcher})
	args := queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: task.ID}
	opts := &river.InsertOpts{MaxAttempts: queueargs.DispatchTaskMaxAttempts}
	wants := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second}
	var result *rivertest.WorkResult
	for index, want := range wants {
		attempt := index + 1
		if index == 0 {
			result, err = testWorker.Work(ctx, t, tx, args, opts)
		} else {
			result, err = testWorker.WorkJob(ctx, t, tx, result.Job)
		}
		if !errors.Is(err, wantFailure) {
			t.Fatalf("attempt %d error=%v, want %v", attempt, err, wantFailure)
		}
		if result.Job.Attempt != attempt || result.Job.MaxAttempts != queueargs.DispatchTaskMaxAttempts || result.Job.AttemptedAt == nil {
			t.Fatalf("attempt %d row=%+v", attempt, result.Job)
		}
		if result.Job.State != rivertype.JobStateRetryable {
			t.Fatalf("attempt %d state=%s, want retryable", attempt, result.Job.State)
		}
		if got := result.Job.ScheduledAt.Sub(*result.Job.AttemptedAt); got < want || got > want+2*time.Second {
			t.Fatalf("attempt %d persisted retry delay=%s, want %s", attempt, got, want)
		}
	}

	result, err = testWorker.WorkJob(ctx, t, tx, result.Job)
	if !errors.Is(err, wantFailure) {
		t.Fatalf("final attempt error=%v, want %v", err, wantFailure)
	}
	if result.Job.Attempt != queueargs.DispatchTaskMaxAttempts || result.Job.MaxAttempts != queueargs.DispatchTaskMaxAttempts ||
		result.Job.State != rivertype.JobStateDiscarded || len(result.Job.Errors) != queueargs.DispatchTaskMaxAttempts {
		t.Fatalf("final River row=%+v", result.Job)
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
