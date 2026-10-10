package postgres

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// An accepted review seat is terminal: it cannot be reclaimed, and its
// persisted state stays completed (component-work-orders).
func TestAcceptedReviewSeatRejectsReclaimIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "accepted-seat-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), workspace)
	cfg := &config.Config{Workspace: workspace, MaxBounces: 2, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	snapshot := &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "exec"}}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, RequiredHarness: "codex", RequiredHarnessConfig: snapshot, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "accepted-session", ClientToken: "accepted-token", WorkerID: "worker", Lease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "approved", Summary: "accepted", PolicyVersion: 1, MergeApproval: false}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "reclaim", ClientToken: "reclaim-token", WorkerID: "worker", Lease: time.Hour}); err == nil || !strings.Contains(err.Error(), "accepted review seat") {
		t.Fatalf("accepted reclaim err=%v", err)
	}
	persisted, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || persisted.State != core.WorkOrderCompleted || persisted.LastAttemptOutcome != "" {
		t.Fatalf("accepted order=%+v err=%v", persisted, err)
	}
}

func TestClaimedReviewVerdictReDerivesRegressedTaskProjectionIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "claim-verdict-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, MaxBounces: 2, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Level: core.L2, PolicyVersion: 1, MergeApproval: true, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning, ModelTier: "reviewer"}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	const session = "owning-review-session"
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: order.ID, ClaimSession: session, ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "approved", Summary: "incident replay", PolicyVersion: 1, MergeApproval: true, MaxBounces: 2}
	if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatalf("accept claimed verdict after projection regression: %v", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting {
		t.Fatalf("task after verdict=%+v err=%v", current, err)
	}
	settledOrder, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || settledOrder.State != core.WorkOrderCompleted {
		t.Fatalf("review order after verdict=%+v err=%v", settledOrder, err)
	}
	settledJobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(settledJobs) != 1 || settledJobs[0].State != core.JobDone || settledJobs[0].EndedAt.IsZero() {
		t.Fatalf("review job after verdict=%+v err=%v", settledJobs, err)
	}

	expiredTask := task
	expiredTask.ID = core.NewTaskID()
	expiredTask.Branch = "conveyor/task-" + expiredTask.ID
	expiredTask.State = core.TaskRunning
	if err = st.CreateTask(ctx, expiredTask); err != nil {
		t.Fatal(err)
	}
	expiredJob := job
	expiredJob.ID, expiredJob.TaskID = expiredTask.ID+"-review-1-seat-1", expiredTask.ID
	if err = st.CreateJob(ctx, expiredJob); err != nil {
		t.Fatal(err)
	}
	expiredOrder := order
	expiredOrder.ID, expiredOrder.TaskID, expiredOrder.JobID = expiredJob.ID, expiredTask.ID, expiredJob.ID
	if err = storetest.For(st).CreateWorkOrder(ctx, expiredOrder); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, expiredOrder.ID, core.WorkOrderClaim{SessionID: session + "-expired", ClientToken: "secret", Lease: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	decision.TaskID, decision.JobID, decision.ReviewWorkOrderID, decision.ClaimSession = expiredTask.ID, expiredJob.ID, expiredOrder.ID, session+"-expired"
	if err = storetest.For(st).AcceptReviewDecision(ctx, decision); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("expired claim verdict err=%v", err)
	}
}
