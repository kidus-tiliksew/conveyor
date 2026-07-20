package dispatch

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
