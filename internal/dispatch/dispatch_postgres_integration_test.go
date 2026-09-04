package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/riverqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type observedTaskLockStore struct {
	store.Store
	orderCreated chan struct{}
	releaseOrder chan struct{}
}

type failingStageOrderStore struct {
	store.Store
	err error
}

type blockingDispatchStore struct {
	store.Store
	taskID  string
	started chan struct{}
	once    sync.Once
}

func (s *blockingDispatchStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	if id != s.taskID {
		return s.Store.GetTask(ctx, id)
	}
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return core.Task{}, ctx.Err()
}

func TestRiverShutdownInterruptionPreservesRowAndAttemptIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "shutdown-interrupt-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Shutdown interrupt "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "shutdown-interrupt-" + suffix, Workspace: workspace, Repo: "repo", Title: "Interrupt dispatch",
		BaseBranch: "main", Branch: "conveyor/shutdown-interrupt-" + suffix,
		State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingDispatchStore{Store: st, taskID: task.ID, started: make(chan struct{})}
	dispatcher := New(blocking, cfg, nil)
	marker := &ShutdownMarker{}
	client, err := testRuntime(t, st.Pool(), dispatcher, marker, []string{workspace}, map[string]*config.Config{workspace: cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-ctx.Done():
		t.Fatal("River dispatch did not start")
	}
	var rowID int64
	if err = st.Pool().QueryRow(ctx, `SELECT id FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`, workspace, task.ID).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err = NewMarkedRuntime(client, marker).StopAndCancel(stopCtx); err != nil {
		t.Fatal(err)
	}
	var persistedID int64
	var state string
	var attempt int
	if err = st.Pool().QueryRow(ctx, `SELECT id, state, attempt FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`, workspace, task.ID).Scan(&persistedID, &state, &attempt); err != nil {
		t.Fatal(err)
	}
	if persistedID != rowID || (state != "available" && state != "retryable" && state != "scheduled") || attempt != 0 {
		t.Fatalf("River row id=%d state=%s attempt=%d, want same id=%d available/retryable with attempt 0", persistedID, state, attempt, rowID)
	}
	if count, countErr := st.CountEvents(taskCtx, task.ID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed events=%d err=%v, want 0", count, countErr)
	}
	current, err := st.GetTask(taskCtx, task.ID)
	if err != nil || current.State != core.TaskQueued {
		t.Fatalf("task=%+v err=%v, want no failure transition", current, err)
	}
}

func TestRiverClientConvergesLateWorkspaceQueuesIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspaceA := "river-converge-a-" + suffix
	workspaceB := "river-converge-b-" + suffix
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspaceA, "River convergence A", dispatchRaceConfig(workspaceA)); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, dispatchRaceConfig(workspaceA), nil)
	client, err := testRuntime(t, st.Pool(), dispatcher, &ShutdownMarker{}, []string{workspaceA}, map[string]*config.Config{workspaceA: dispatchRaceConfig(workspaceA)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := client.Client().Subscribe(river.EventKindJobCompleted)
	defer unsubscribe()
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if stopErr := client.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop River client: %v", stopErr)
		}
	}()

	waitForRiverJob(t, ctx, events, func(job *rivertype.JobRow) bool {
		return job.Kind == (queueargs.OrderClockArgs{}).Kind() &&
			job.Queue == queueargs.ControlQueue && riverJobWorkspace(job) == workspaceA
	}, "startup workspace order clock")

	if _, err = st.CreateWorkspace(actorCtx, workspaceB, "River convergence B", dispatchRaceConfig(workspaceB)); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspaceB)
	parent := core.Task{
		ID: "river-converge-parent-" + suffix, Workspace: workspaceB, Repo: "repo", Title: "River convergence parent",
		BaseBranch: "main", Branch: "conveyor/river-converge-parent-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	child := core.Task{
		ID: "river-converge-child-" + suffix, Workspace: workspaceB, Repo: "repo", Title: "River convergence child",
		BaseBranch: "main", Branch: "conveyor/river-converge-child-" + suffix,
		State: core.TaskQueued, NextStage: core.StageImplement, ParentTaskID: parent.ID,
		OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, parent); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(taskCtx, child); err != nil {
		t.Fatal(err)
	}
	registrar := NewWorkspaceQueueRegistrar([]string{workspaceA}, func(workspace string) error {
		return client.EnsureWorkspace(workspace)
	}, t.Logf)
	workspaces, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var convergenceWorkspaces []core.Workspace
	for _, workspace := range workspaces {
		if workspace.ID == workspaceA || workspace.ID == workspaceB {
			convergenceWorkspaces = append(convergenceWorkspaces, workspace)
		}
	}
	if len(convergenceWorkspaces) != 2 {
		t.Fatalf("convergence workspaces=%v, want %s and %s", convergenceWorkspaces, workspaceA, workspaceB)
	}
	if err = registrar.Converge(convergenceWorkspaces); err != nil {
		t.Fatal(err)
	}

	dispatchWorked := false
	orderClockWorked := false
	for !dispatchWorked || !orderClockWorked {
		select {
		case <-ctx.Done():
			t.Fatalf("wait for late workspace jobs: dispatch=%t order_clock=%t: %v", dispatchWorked, orderClockWorked, ctx.Err())
		case event := <-events:
			job := event.Job
			if job == nil || riverJobWorkspace(job) != workspaceB {
				continue
			}
			dispatchWorked = dispatchWorked || (job.Kind == (queueargs.DispatchTaskArgs{}).Kind() &&
				job.Queue == queueargs.DispatchQueue(workspaceB))
			orderClockWorked = orderClockWorked || (job.Kind == (queueargs.OrderClockArgs{}).Kind() &&
				job.Queue == queueargs.ControlQueue)
		}
	}
}

func waitForRiverJob(t *testing.T, ctx context.Context, events <-chan *river.Event, matches func(*rivertype.JobRow) bool, description string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v", description, ctx.Err())
		case event := <-events:
			if event.Job != nil && matches(event.Job) {
				return
			}
		}
	}
}

func riverJobWorkspace(job *rivertype.JobRow) string {
	var args struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if json.Unmarshal(job.EncodedArgs, &args) != nil {
		return ""
	}
	return args.WorkspaceID
}

func TestWorkspaceForgeOperationFailsClosedWithoutTokenIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "forge-fail-closed-" + core.NewTaskID()
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Forge fail closed", cfg); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	_, err = dispatcher.PublishIssue(store.WithWorkspace(ctx, workspace), githubtrigger.IssuePublication{
		Repo: "acme/repo", TaskID: "task-without-workspace-token", Title: "must fail before GitHub",
	})
	if githubtrigger.ErrorCategory(err) != githubtrigger.ForgePermission {
		t.Fatalf("category = %q, error = %v", githubtrigger.ErrorCategory(err), err)
	}
	if !strings.Contains(err.Error(), "workspace "+workspace+" forge token") || !strings.Contains(err.Error(), "add or replace it in workspace settings") {
		t.Fatalf("permission remedy = %v", err)
	}
}

func TestApproverTokenMergeWaitAndRetryIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "approver-merge-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Approver merge "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "approver-merge-" + suffix, Workspace: workspace, Repo: "repo", Title: "Approver merge",
		BaseBranch: "main", Branch: "conveyor/approver-merge-" + suffix,
		State: core.TaskApproved, MergeApproval: true, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.BindTaskApproval(taskCtx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateIntervention(taskCtx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	forgeTokens := &mergeForgeTokens{status: core.ForgeTokenStatus{Configured: false}}
	dispatcher := New(st, cfg, nil)
	dispatcher.ForgeTokens = forgeTokens
	if err = dispatcher.MergeApprovedTask(taskCtx, task); err == nil || !strings.Contains(err.Error(), "account settings") {
		t.Fatalf("missing-token merge error=%v", err)
	}
	current, getErr := st.GetTask(taskCtx, task.ID)
	if getErr != nil || current.State != core.TaskApproved || !current.MergeApproval {
		t.Fatalf("approval changed after refusal: task=%+v err=%v", current, getErr)
	}
	events, eventErr := st.ListEvents(taskCtx, task.ID)
	if eventErr != nil {
		t.Fatal(eventErr)
	}
	refusalFound := false
	for _, event := range events {
		if event.Kind != "merge.failed" {
			continue
		}
		var author struct {
			Class  core.ForgeAuthorClass `json:"forge_author_class"`
			UserID string                `json:"forge_author_user_id"`
		}
		if err = json.Unmarshal(event.Payload, &author); err != nil {
			t.Fatal(err)
		}
		refusalFound = author.Class == core.ForgeAuthorApprovingOperator && author.UserID == "usr-approver"
	}
	if !refusalFound {
		t.Fatalf("approver-attributed refusal not found: %+v", events)
	}

	forgeTokens.status = core.ForgeTokenStatus{Configured: true}
	forgeTokens.credential = core.ForgeTokenCredential{UserID: "usr-approver", Token: "approver-forge-token"}
	views := 0
	dispatcher.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 12, State: map[bool]string{false: "open", true: "closed"}[views > 1], Mergeable: "MERGEABLE", Merged: views > 1, BaseSHA: "base", HeadSHA: "approved-head"}, nil
	}
	dispatcher.RequestMergeWithCredential = func(_ context.Context, _ string, _ int, token string) error {
		if token != "approver-forge-token" {
			t.Fatalf("merge token=%q", token)
		}
		return nil
	}
	if err = dispatcher.MergeApprovedTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	current, getErr = st.GetTask(taskCtx, task.ID)
	if getErr != nil || current.State != core.TaskMerged {
		t.Fatalf("post-token retry task=%+v err=%v", current, getErr)
	}
}

func TestRiverDispatchCompletesBlueprintAnchorIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "dispatch-blueprint-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Dispatch blueprint "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	parent := core.Task{
		ID: "blueprint-parent-" + suffix, Workspace: workspace, Repo: "repo", Title: "Blueprint",
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

	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `DELETE FROM river_job
		WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`,
		workspace, parent.ID); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	testWorker := rivertest.NewWorker(t, riverpgxv5.New(nil), &river.Config{ID: "blueprint-anchor"}, riverqueue.WorkerFor[queueargs.DispatchTaskArgs](dispatchRegistration(dispatcher), nil))
	result, err := testWorker.Work(ctx, t, tx,
		queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: parent.ID},
		&river.InsertOpts{MaxAttempts: queueargs.DispatchTaskMaxAttempts})
	if err != nil {
		t.Fatalf("blueprint dispatch returned %v, want completion", err)
	}
	if result.Job.State != rivertype.JobStateCompleted || result.Job.Attempt != 1 {
		t.Fatalf("blueprint River row state=%s attempt=%d, want completed attempt 1", result.Job.State, result.Job.Attempt)
	}
	persisted, err := st.GetTask(taskCtx, parent.ID)
	if err != nil || persisted.State != core.TaskQueued {
		t.Fatalf("blueprint parent=%+v err=%v", persisted, err)
	}
	orders, err := st.ListTaskWorkOrders(taskCtx, parent.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("blueprint parent orders=%+v err=%v", orders, err)
	}
}

func (s *failingStageOrderStore) CreateStageWorkOrderCommand(context.Context, taskops.TaskLease, core.Job, core.WorkOrder) (bool, error) {
	return false, s.err
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
	dispatcher := New(&failingStageOrderStore{Store: st, err: wantFailure}, cfg, nil)
	testWorker := rivertest.NewWorker(t, riverpgxv5.New(nil), &river.Config{ID: "dispatch-retry-policy"}, riverqueue.WorkerFor[queueargs.DispatchTaskArgs](dispatchRegistration(dispatcher), nil))
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

func (s *observedTaskLockStore) CreateConflictFixCommand(ctx context.Context, lease taskops.TaskLease, request store.ConflictFixRequest) (store.ConflictFixResult, error) {
	result, err := s.Store.CreateConflictFixCommand(ctx, lease, request)
	if err != nil {
		return store.ConflictFixResult{}, err
	}
	if result.Created {
		close(s.orderCreated)
		<-s.releaseOrder
	}
	return result, nil
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
		Store: st, orderCreated: make(chan struct{}), releaseOrder: make(chan struct{}),
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
		riverDone <- worker.Work(ctx, testJob(queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: task.ID}, 1, 1, 5))
	}()
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
		var snooze *queueargs.SnoozeError
		if !errors.As(err, &snooze) || snooze.Duration != time.Second {
			t.Fatalf("River dispatch result=%v, want one-second stage snooze", err)
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
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("task after overlap = %+v, err = %v", current, err)
	}

	claims := make(chan error, 2)
	for i := range 2 {
		go func() {
			_, claimErr := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{
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

func TestConflictFixCommandRollsBackPostgresOnOrderCreationFailureIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := t.Context()
	st, err := storepg.Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "conflict-atomic-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Conflict atomic "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	task := core.Task{
		ID: "conflict-atomic-" + suffix, Workspace: workspace, Repo: "repo", Title: "Atomic conflict fix",
		BaseBranch: "main", Branch: "conveyor/conflict-atomic-" + suffix,
		State: core.TaskApproved, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}

	// Pre-create only the job identity so the command fails at durable order
	// materialization after attempting its earlier writes. The Postgres
	// transaction must roll all of those attempted writes back.
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := store.ConflictFixRequest{
		TaskID: task.ID,
		Job:    job,
		WorkOrder: core.WorkOrder{
			ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement,
			State: core.WorkOrderQueued, ReasonCode: "merge-conflict",
			QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
		},
		Intervention: core.Intervention{
			TaskID: task.ID, ActorID: "system", ActorRole: core.ActorSystem,
			Action: core.InterventionRedirect, ReasonCode: "merge-conflict",
		},
		ApprovedHead: "approved-head", NewHead: "conflicting-head",
	}
	beforeTransitions, err := st.CountEvents(ctx, task.ID, "task.state_changed")
	if err != nil {
		t.Fatal(err)
	}
	systemCtx := store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem})
	_, err = taskops.ExecuteWorkOrder(systemCtx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ConflictFixResult, error) {
		return st.CreateConflictFixCommand(systemCtx, lease, request)
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("command error=%v, want duplicate job failure", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved || current.NextStage != core.StageReview {
		t.Fatalf("task after rollback=%+v err=%v", current, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("orders after rollback=%+v err=%v", orders, err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 0 {
		t.Fatalf("interventions after rollback=%+v err=%v", interventions, err)
	}
	for kind, want := range map[string]int{
		"task.state_changed":            beforeTransitions,
		"pipeline.transition_decided":   0,
		"merge.conflict_fix_dispatched": 0,
	} {
		if count, countErr := st.CountEvents(ctx, task.ID, kind); countErr != nil || count != want {
			t.Fatalf("%s events=%d want=%d err=%v", kind, count, want, countErr)
		}
	}
}

func TestReviewPublicationWorkerPostgresProjectionLifecycleIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := t.Context()
	st, err := storepg.Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "review-publication-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Review publication "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	task := core.Task{
		ID: "review-publication-" + suffix, Workspace: workspace, Repo: "repo",
		Title: "Publish review", Branch: "conveyor/review-publication-" + suffix,
		BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{
		TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: job.ID,
		Verdict: "approve", ReasonCode: "approved", Summary: "All criteria pass.",
		ReviewedCommitSHA: "head-1", ReviewerModel: "reviewer", ReviewerSession: "review-session",
		SameModelAsImplementer: "false", PublicationEligible: true,
		ReviewRound: 1, ReviewSeat: 1, PolicyVersion: 1, MergeApproval: true, MaxBounces: 3,
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	publication, err := st.GetReviewPublication(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var completedAt time.Time
	for _, event := range events {
		if event.Kind == "review.completed" {
			completedAt = event.At
			break
		}
	}
	if completedAt.IsZero() || !completedAt.After(publication.CreatedAt) {
		t.Fatalf("production ordering not reproduced: completed_at=%s publication_created_at=%s", completedAt, publication.CreatedAt)
	}

	dispatcher := New(st, cfg, nil)
	var requests []githubtrigger.ReviewPublication
	dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		requests = append(requests, request)
		return githubtrigger.ReviewPublicationResult{CommentID: 71, ReviewedCommitSHA: "head-1"}, nil
	}
	worker := &reviewPublicationWorker{dispatcher: dispatcher}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, job.ID, 1)); err != nil {
		t.Fatal(err)
	}
	publication, err = st.GetReviewPublication(ctx, job.ID)
	if err != nil || publication.State != core.ReviewPublicationPublished || publication.CommentID != 71 {
		t.Fatalf("single-seat publication=%+v err=%v", publication, err)
	}
	if len(requests) != 1 || len(requests[0].History) != 1 || requests[0].History[0].WorkOrderID != job.ID {
		t.Fatalf("single-seat requests=%+v", requests)
	}

	panelTask := core.Task{
		ID: "review-panel-" + suffix, Workspace: workspace, Repo: "repo", Title: "Publish panel",
		Branch: "conveyor/review-panel-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, panelTask); err != nil {
		t.Fatal(err)
	}
	panelIDs := []string{panelTask.ID + "-review-1-seat-1", panelTask.ID + "-review-1-seat-2"}
	for index, id := range panelIDs {
		if err = st.CreateJob(ctx, core.Job{ID: id, TaskID: panelTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
			ReviewWorkOrderID: id, TaskID: panelTask.ID, JobID: id, Verdict: "approve",
			ReasonCode: "approved", Summary: "Seat passes.", ReviewRound: 1, ReviewSeat: index + 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: panelTask.ID, JobID: id, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": id, "verdict": "approve", "reason_code": "approved",
			"summary": "Seat passes.", "review_round": 1, "review_seat": index + 1,
		})}); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
				requests = append(requests, request)
				return githubtrigger.ReviewPublicationResult{CommentID: 81, ReviewedCommitSHA: "panel-head"}, nil
			}
			if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, id, 1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: panelTask.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
		"review_round": 1, "verdict": "approve",
	})}); err != nil {
		t.Fatal(err)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, panelIDs[1], 1)); err != nil {
		t.Fatal(err)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, panelIDs[0], 2)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[1].StatusState != "pending" || requests[2].StatusState != "success" ||
		len(requests[2].History) != 2 {
		t.Fatalf("panel requests=%+v", requests)
	}
	for _, id := range panelIDs {
		stored, getErr := st.GetReviewPublication(ctx, id)
		if getErr != nil || stored.State != core.ReviewPublicationPublished || stored.CommentID != 81 {
			t.Fatalf("panel publication %s=%+v err=%v", id, stored, getErr)
		}
	}

	roundTask := core.Task{
		ID: "review-rounds-" + suffix, Workspace: workspace, Repo: "repo", Title: "Publish review rounds",
		Branch: "conveyor/review-rounds-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, roundTask); err != nil {
		t.Fatal(err)
	}
	roundIDs := []string{roundTask.ID + "-review-1", roundTask.ID + "-review-2"}
	roundVerdicts := []string{"changes_requested", "approve"}
	var roundRequests []githubtrigger.ReviewPublication
	dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		roundRequests = append(roundRequests, request)
		return githubtrigger.ReviewPublicationResult{CommentID: 121, ReviewedCommitSHA: "round-head"}, nil
	}
	for index, id := range roundIDs {
		round := index + 1
		verdict := roundVerdicts[index]
		reasonCode := "tests"
		feedback := "Add coverage."
		if verdict == "approve" {
			reasonCode = "approved"
			feedback = ""
		}
		if err = st.CreateJob(ctx, core.Job{ID: id, TaskID: roundTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
			ReviewWorkOrderID: id, TaskID: roundTask.ID, JobID: id, Verdict: verdict,
			ReasonCode: reasonCode, Summary: "Round complete.", Feedback: feedback,
			ReviewRound: round, ReviewSeat: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: roundTask.ID, JobID: id, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": id, "verdict": verdict, "reason_code": reasonCode,
			"summary": "Round complete.", "feedback": feedback, "review_round": round, "review_seat": 1,
		})}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: roundTask.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
			"review_round": round, "verdict": verdict,
		})}); err != nil {
			t.Fatal(err)
		}
		if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if len(roundRequests) != 2 || len(roundRequests[0].History) != 1 ||
		roundRequests[0].History[0].ResolutionState != "unresolved" ||
		len(roundRequests[1].History) != 2 ||
		roundRequests[1].History[0].ResolutionState != "resolved" ||
		roundRequests[1].History[1].ResolutionState != "accepted" {
		t.Fatalf("later-round requests=%+v", roundRequests)
	}
	for _, id := range roundIDs {
		stored, getErr := st.GetReviewPublication(ctx, id)
		if getErr != nil || stored.State != core.ReviewPublicationPublished || stored.CommentID != 121 {
			t.Fatalf("later-round publication %s=%+v err=%v", id, stored, getErr)
		}
	}

	failureTask := core.Task{
		ID: "review-failure-" + suffix, Workspace: workspace, Repo: "repo", Title: "Retry publication",
		Branch: "conveyor/review-failure-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, failureTask); err != nil {
		t.Fatal(err)
	}
	failureID := failureTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: failureID, TaskID: failureTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
		ReviewWorkOrderID: failureID, TaskID: failureTask.ID, JobID: failureID,
		Verdict: "approve", ReasonCode: "approved", Summary: "Passes.",
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("GitHub comment unavailable")
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{}, wantErr
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, failureID, 1)); !errors.Is(err, wantErr) {
		t.Fatalf("publication failure=%v, want %v", err, wantErr)
	}
	failedPublication, err := st.GetReviewPublication(ctx, failureID)
	if err != nil || failedPublication.State != core.ReviewPublicationRetrying || failedPublication.CommentID != 0 {
		t.Fatalf("retrying publication=%+v err=%v", failedPublication, err)
	}
	authoritativeTask, err := st.GetTask(ctx, failureTask.ID)
	if err != nil || authoritativeTask.State != core.TaskRunning || authoritativeTask.NextStage != core.StageReview {
		t.Fatalf("internal review authority changed: task=%+v err=%v", authoritativeTask, err)
	}
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{CommentID: 91, ReviewedCommitSHA: "retry-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, failureID, 2)); err != nil {
		t.Fatal(err)
	}
	failedPublication, err = st.GetReviewPublication(ctx, failureID)
	if err != nil || failedPublication.State != core.ReviewPublicationPublished || failedPublication.CommentID != 91 ||
		failedPublication.Attempts != 2 {
		t.Fatalf("retried publication=%+v err=%v", failedPublication, err)
	}

	reconcileTask := core.Task{
		ID: "review-reconcile-" + suffix, Workspace: workspace, Repo: "repo", Title: "Reconcile publication",
		Branch: "conveyor/review-reconcile-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, reconcileTask); err != nil {
		t.Fatal(err)
	}
	reconcileID := reconcileTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: reconcileID, TaskID: reconcileTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: reconcileTask.ID, JobID: reconcileID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": reconcileID, "verdict": "approve", "reason_code": "approved",
		"summary": "Reconciled.", "publication_eligible": true,
	})}); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("reconciled publications=%d err=%v", repaired, reconcileErr)
	}
	reconcileCalls := 0
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		reconcileCalls++
		return githubtrigger.ReviewPublicationResult{CommentID: 101, ReviewedCommitSHA: "reconcile-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, reconcileID, 1)); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("duplicate reconciliation publications=%d err=%v", repaired, reconcileErr)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, reconcileID, 2)); err != nil {
		t.Fatal(err)
	}
	reconciledPublication, err := st.GetReviewPublication(ctx, reconcileID)
	if err != nil || reconciledPublication.State != core.ReviewPublicationPublished ||
		reconciledPublication.CommentID != 101 || reconcileCalls != 1 {
		t.Fatalf("reconciled publication=%+v calls=%d err=%v", reconciledPublication, reconcileCalls, err)
	}

	legacyTask := core.Task{
		ID: "review-legacy-" + suffix, Workspace: workspace, Repo: "repo", Title: "Repair legacy publication",
		Branch: "conveyor/review-legacy-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, legacyTask); err != nil {
		t.Fatal(err)
	}
	legacyID := legacyTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: legacyID, TaskID: legacyTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
		ReviewWorkOrderID: legacyID, TaskID: legacyTask.ID, JobID: legacyID,
		Verdict: "approve", ReasonCode: "approved", Summary: "Legacy projection.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Pool().Exec(ctx, `UPDATE review_publications
		SET state='published', comment_id=0 WHERE workspace_id=$1 AND review_work_order_id=$2`,
		workspace, legacyID); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("legacy projection repairs=%d err=%v", repaired, reconcileErr)
	}
	legacyPublication, err := st.GetReviewPublication(ctx, legacyID)
	if err != nil || legacyPublication.State != core.ReviewPublicationRetrying || legacyPublication.CommentID != 0 {
		t.Fatalf("legacy publication after repair=%+v err=%v", legacyPublication, err)
	}
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{CommentID: 111, ReviewedCommitSHA: "legacy-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, legacyID, 1)); err != nil {
		t.Fatal(err)
	}
	legacyPublication, err = st.GetReviewPublication(ctx, legacyID)
	if err != nil || legacyPublication.State != core.ReviewPublicationPublished || legacyPublication.CommentID != 111 {
		t.Fatalf("repaired legacy publication=%+v err=%v", legacyPublication, err)
	}
}

func reviewPublicationIntegrationJob(workspace, workOrderID string, attempt int) queueargs.Job {
	return testJob(queueargs.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: workOrderID}, int64(attempt), attempt, 5)
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
