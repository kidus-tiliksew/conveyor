package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type observedTaskLockStore struct {
	store.Store
	orderCreated chan struct{}
	releaseOrder chan struct{}
}

func TestApproverMergeWithoutStoredTokenIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 30*time.Second)
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
	dispatcher := New(mergeIdentityStore{st}, cfg, nil)
	views := 0
	dispatcher.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 12, State: map[bool]string{false: "open", true: "closed"}[views > 1], Mergeable: "MERGEABLE", Merged: views > 1, BaseSHA: "base", HeadSHA: "approved-head"}, nil
	}
	dispatcher.RequestMerge = func(_ context.Context, _ string, _ int) error {
		return nil
	}
	if err = dispatcher.MergeApprovedTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	current, getErr := st.GetTask(taskCtx, task.ID)
	if getErr != nil || current.State != core.TaskMerged {
		t.Fatalf("post-token retry task=%+v err=%v", current, getErr)
	}
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

func TestConflictFixAndQueueDispatchSerializeIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := store.WithActor(t.Context(), store.SystemActor())
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

	queueDone := make(chan error, 1)
	worker := &dispatchTaskWorker{dispatcher: dispatcher}
	go func() {
		queueDone <- worker.Work(ctx, testJob(queue.DispatchTaskArgs{WorkspaceID: workspace, TaskID: task.ID}, 1, 1, 5))
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
	case err = <-queueDone:
		// The conflict fix left the task queued with its implement order
		// waiting for a claimant, so the dispatch job completes.
		if err != nil {
			t.Fatalf("queue dispatch result=%v, want completion", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queue dispatch did not finish")
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
	root := store.WithActor(t.Context(), store.SystemActor())
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
	if !errors.Is(err, store.ErrDispatchJobConflict) {
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
