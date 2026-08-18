package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresWorkOrderContinuationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	workspace := "continuation-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createClaimed := func(id string) core.WorkOrder {
		t.Helper()
		task := core.Task{ID: id, Workspace: workspace, Repo: "app", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: now}
		job := core.Job{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
		if createErr := st.CreateTask(ctx, task); createErr != nil {
			t.Fatal(createErr)
		}
		if createErr := st.CreateJob(ctx, job); createErr != nil {
			t.Fatal(createErr)
		}
		if createErr := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); createErr != nil {
			t.Fatal(createErr)
		}
		claimed, claimErr := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
			SessionID: id + "-session", ClientToken: id + "-secret", ClaimantID: "worker-a", WorkerID: "worker-a", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour,
		})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		return claimed
	}

	terminal := createClaimed("continuation-terminal")
	identity := core.WorkOrderClaimIdentity{WorkerID: terminal.WorkerID, ClaimantID: terminal.ClaimantID, SessionID: terminal.SessionID}
	first := core.WorkOrderContinuation{SessionID: "native-1", AttemptID: terminal.AttemptID, Harness: "codex", LaunchEnvironment: "worker-a/env-1"}
	if _, err = st.RecordWorkOrderContinuation(ctx, terminal.ID, identity, first); err != nil {
		st.Close()
		t.Fatal(err)
	}
	second := first
	second.SessionID = "native-2"
	if _, err = st.RecordWorkOrderContinuation(ctx, terminal.ID, identity, second); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx = store.WithWorkspace(t.Context(), workspace)
	persisted, err := st.GetWorkOrder(ctx, terminal.ID)
	if err != nil || persisted.ContinuationSessionID != "native-2" || persisted.ContinuationAttemptID != terminal.AttemptID ||
		persisted.ContinuationHarness != "codex" || persisted.ContinuationLaunchEnvironment != "worker-a/env-1" {
		t.Fatalf("persisted continuation=%+v err=%v", persisted, err)
	}
	persisted.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, persisted, core.WorkOrderCmdSubmitSpec); err != nil {
		t.Fatal(err)
	}
	cleared, err := st.GetWorkOrder(ctx, terminal.ID)
	if err != nil || cleared.ContinuationSessionID != "" || cleared.ContinuationAttemptID != "" ||
		cleared.ContinuationHarness != "" || cleared.ContinuationLaunchEnvironment != "" {
		t.Fatalf("terminal continuation=%+v err=%v", cleared, err)
	}

	resumable := createClaimed("continuation-resumable")
	resumeIdentity := core.WorkOrderClaimIdentity{WorkerID: resumable.WorkerID, ClaimantID: resumable.ClaimantID, SessionID: resumable.SessionID}
	capture := core.WorkOrderContinuation{SessionID: "native-resume", AttemptID: resumable.AttemptID, Harness: "codex", LaunchEnvironment: "worker-a/env-1"}
	if _, err = st.RecordWorkOrderContinuation(ctx, resumable.ID, resumeIdentity, capture); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ReleaseWorkerClaim(ctx, resumable.ID, "worker-a", core.WorkOrderRelease{
		SessionID: resumable.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).RecoverWorkOrder(ctx, resumable.ID, "continuation-recovery", time.Hour); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := storetest.For(st).ClaimWorkOrder(ctx, resumable.ID, core.WorkOrderClaim{
		SessionID: "resumed-claim", ClientToken: "resumed-secret", ClaimantID: "worker-a", WorkerID: "worker-a", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil || !reclaimed.CanResumeContinuation() || reclaimed.ContinuationSessionID != capture.SessionID {
		t.Fatalf("reclaimed continuation=%+v err=%v", reclaimed, err)
	}
}

func TestPostgresAcceptedReviewClearsSubmittedImplementationContinuationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "continuation-review-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "app", Branch: "conveyor/task-review-continuation", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	implementJob := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, implementJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: implementJob.ID, TaskID: task.ID, JobID: implementJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	implement, err := storetest.For(st).ClaimWorkOrder(ctx, implementJob.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", ClaimantID: "run:implementer", WorkerID: "worker-implement", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	identity := core.WorkOrderClaimIdentity{WorkerID: implement.WorkerID, ClaimantID: implement.ClaimantID, SessionID: implement.SessionID}
	if _, err = st.RecordWorkOrderContinuation(ctx, implement.ID, identity, core.WorkOrderContinuation{SessionID: "native-session", AttemptID: implement.AttemptID, Harness: "codex", LaunchEnvironment: "worker-implement/env"}); err != nil {
		t.Fatal(err)
	}
	implement.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, implement, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	reviewOrder := core.WorkOrder{ID: reviewJob.ID, TaskID: task.ID, JobID: reviewJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = st.CreateJob(ctx, reviewJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, reviewOrder); err != nil {
		t.Fatal(err)
	}
	claimedReview, err := storetest.For(st).ClaimWorkOrder(ctx, reviewOrder.ID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "review-token", ClaimantID: "run:reviewer", WorkerID: "worker-review", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: reviewJob.ID, ReviewWorkOrderID: reviewOrder.ID, ClaimSession: claimedReview.SessionID, ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "approved", Summary: "accepted", PolicyVersion: 1, MergeApproval: false}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetWorkOrder(ctx, implement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ContinuationSessionID != "" || persisted.ContinuationAttemptID != "" || persisted.ContinuationHarness != "" || persisted.ContinuationLaunchEnvironment != "" {
		t.Fatalf("submitted implementation continuation was not cleared: %+v", persisted)
	}
}
