package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPhase47PersistenceIntegration(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase47-" + core.NewTaskID()
	cfg := &config.Config{Workspace: workspace, MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt-5.4", Execution: config.ExecutionInProcess, TimeoutText: "1m", Timeout: time.Minute},
		"spec":      {Model: "gpt-5.4", Execution: config.ExecutionInProcess, TimeoutText: "1m", Timeout: time.Minute},
		"implement": {Model: "operator", Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour},
		"review":    {Model: "operator", Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour},
	}}, Repos: []config.Repo{{Name: "api", URL: "https://example.test/api.git", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	feature := core.Feature{ID: "feature-" + core.NewTaskID(), Name: "Exports"}
	if err = st.CreateFeature(ctx, feature); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "api", Title: "Audit export", Source: "test", IntakeKey: "issue-42", BaseBranch: "main", Branch: "conveyor/integration-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if found, ok, getErr := st.GetTaskByIntakeKey(ctx, "issue-42"); getErr != nil || !ok || found.ID != task.ID {
		t.Fatalf("idempotent task=%+v ok=%t err=%v", found, ok, getErr)
	}
	duplicate := task
	duplicate.ID = core.NewTaskID()
	duplicate.Branch = task.Branch + "-duplicate"
	if err = st.CreateTask(ctx, duplicate); err == nil {
		t.Fatal("duplicate workspace intake key succeeded")
	}
	if err = st.AssignTaskFeature(ctx, task.ID, feature.ID); err != nil {
		t.Fatal(err)
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "brief.txt", ContentType: "text/plain", TaskID: task.ID}, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID == "" || artifact.SizeBytes != 5 {
		t.Fatalf("artifact = %+v", artifact)
	}
	_, content, err := st.GetArtifact(ctx, artifact.ID)
	if err != nil || string(content) != "brief" {
		t.Fatalf("artifact content=%q err=%v", content, err)
	}
	for _, job := range []core.Job{{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}, {ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending, StartedAt: time.Now().Add(time.Second)}} {
		if err = st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
	}
	claim := core.WorkOrderClaim{SessionID: "session-a", ClientToken: "token-a", Agent: "codex", Model: "gpt", Lease: time.Minute}
	if _, err = st.ClaimWorkOrder(ctx, task.ID+"-implement", claim); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimWorkOrder(ctx, task.ID+"-review", claim); err == nil || !strings.Contains(err.Error(), "self-review forbidden") {
		t.Fatalf("self review error = %v", err)
	}

	clockTaskID := core.NewTaskID()
	clockTask := core.Task{ID: clockTaskID, Workspace: workspace, Repo: "api", Title: "Work-order clocks", BaseBranch: "main", Branch: "conveyor/clocks-" + clockTaskID, State: core.TaskRunning, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, clockTask); err != nil {
		t.Fatal(err)
	}
	clockJob := core.Job{ID: clockTaskID + "-implement", TaskID: clockTaskID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, clockJob); err != nil {
		t.Fatal(err)
	}
	queuedAt := time.Now().Add(-2 * time.Hour)
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: clockJob.ID, TaskID: clockTaskID, JobID: clockJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: queuedAt, QueueDeadline: queuedAt.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	clockClaim, err := st.ClaimWorkOrder(ctx, clockJob.ID, core.WorkOrderClaim{SessionID: "clock-session", ClientToken: "clock-token", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || clockClaim.ExecutionStartedAt.IsZero() || clockClaim.ExecutionDeadline.Sub(clockClaim.ExecutionStartedAt) != time.Hour {
		t.Fatalf("clock claim=%+v err=%v", clockClaim, err)
	}
	clockClaim.ExecutionDeadline = time.Now().Add(-time.Second)
	if err = st.UpdateWorkOrder(ctx, clockClaim); err != nil {
		t.Fatal(err)
	}
	if timedOut, getErr := st.GetWorkOrder(ctx, clockJob.ID); getErr != nil || timedOut.State != core.WorkOrderTimedOut || timedOut.Claimable {
		t.Fatalf("timed out=%+v err=%v", timedOut, getErr)
	}
	clockClaim.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, clockClaim); !errors.Is(err, store.ErrWorkOrderTimedOut) {
		t.Fatalf("stale timed-out update error=%v", err)
	}
	if timedOut, getErr := st.GetWorkOrder(ctx, clockJob.ID); getErr != nil || timedOut.State != core.WorkOrderTimedOut {
		t.Fatalf("timed out after stale update=%+v err=%v", timedOut, getErr)
	}

	staleTaskID := core.NewTaskID()
	staleTask := core.Task{ID: staleTaskID, Workspace: workspace, Repo: "api", Title: "Stale work order", BaseBranch: "main", Branch: "conveyor/stale-" + staleTaskID, State: core.TaskRunning, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, staleTask); err != nil {
		t.Fatal(err)
	}
	staleJob := core.Job{ID: staleTaskID + "-review", TaskID: staleTaskID, Stage: core.StageReview, State: core.JobPending}
	if err = st.CreateJob(ctx, staleJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: staleJob.ID, TaskID: staleTaskID, JobID: staleJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().Add(-2 * time.Hour), QueueDeadline: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if stale, getErr := st.GetWorkOrder(ctx, staleJob.ID); getErr != nil || stale.State != core.WorkOrderStale || stale.Claimable {
		t.Fatalf("stale=%+v err=%v", stale, getErr)
	}
	redispatched, err := st.RedispatchWorkOrder(ctx, staleJob.ID, 24*time.Hour)
	if err != nil || redispatched.State != core.WorkOrderQueued || !redispatched.Claimable || redispatched.RedispatchCount != 1 || !redispatched.ExecutionStartedAt.IsZero() {
		t.Fatalf("redispatched=%+v err=%v", redispatched, err)
	}
	publication := core.ReviewPublication{ReviewWorkOrderID: task.ID + "-review", TaskID: task.ID, JobID: task.ID + "-review", Verdict: "approve", ReasonCode: "approved", Summary: "passes", ReviewerModel: "gpt", ReviewerSession: "distinct", SameModelAsImplementer: "true"}
	if err = st.QueueReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	storedPublication, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || storedPublication.State != core.ReviewPublicationQueued {
		t.Fatalf("publication=%+v err=%v", storedPublication, err)
	}
	storedPublication.State = core.ReviewPublicationPublished
	storedPublication.Attempts = 1
	storedPublication.CheckRunID = 41
	storedPublication.CommentID = 51
	if err = st.UpdateReviewPublication(ctx, storedPublication); err != nil {
		t.Fatal(err)
	}
	storedPublication, err = st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || storedPublication.State != core.ReviewPublicationPublished || storedPublication.CheckRunID != 41 || storedPublication.CommentID != 51 {
		t.Fatalf("published=%+v err=%v", storedPublication, err)
	}
	inProcessJob := core.Job{ID: task.ID + "-review-in-process", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer", StartedAt: time.Now().Add(2 * time.Second)}
	if err = st.CreateJob(ctx, inProcessJob); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: inProcessJob.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": inProcessJob.ID, "verdict": "approve", "reason_code": "approved",
		"summary": "in-process passes", "reviewer_model": "reviewer", "reviewer_session": "distinct",
		"same_model_as_implementer": "false", "publication_eligible": true,
	})}); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("reconciled in-process publications=%d err=%v", repaired, reconcileErr)
	}
	if stored, getErr := st.GetReviewPublication(ctx, inProcessJob.ID); getErr != nil || stored.ReviewWorkOrderID != inProcessJob.ID {
		t.Fatalf("in-process publication=%+v err=%v", stored, getErr)
	}

	atomicTaskID := core.NewTaskID()
	atomicTask := core.Task{ID: atomicTaskID, Workspace: workspace, Repo: "api", Title: "Atomic review", Source: "test", Level: core.L0, BaseBranch: "main", Branch: "conveyor/atomic-" + atomicTaskID, State: core.TaskRunning, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, atomicTask); err != nil {
		t.Fatal(err)
	}
	atomicJob := core.Job{ID: atomicTaskID + "-review", TaskID: atomicTaskID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer", StartedAt: time.Now()}
	if err = st.CreateJob(ctx, atomicJob); err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{
		TaskID: atomicTaskID, JobID: atomicJob.ID, ReviewWorkOrderID: atomicJob.ID,
		Verdict: "invalid", ReasonCode: "approved", Summary: "passes",
		Reviewer: "integration", ReviewerModel: "reviewer", ReviewerSession: "distinct",
		SameModelAsImplementer: "false", PublicationEligible: true, Level: core.L0, MaxBounces: 2,
	}
	if err = st.AcceptReviewDecision(ctx, decision); err == nil {
		t.Fatal("invalid publication verdict unexpectedly committed")
	}
	if count, countErr := st.CountEvents(ctx, atomicTaskID, "review.completed"); countErr != nil || count != 0 {
		t.Fatalf("review.completed after rollback=%d err=%v", count, countErr)
	}
	if current, getErr := st.GetTask(ctx, atomicTaskID); getErr != nil || current.State != core.TaskRunning {
		t.Fatalf("atomic task after rollback=%+v err=%v", current, getErr)
	}
	if _, getErr := st.GetReviewPublication(ctx, atomicJob.ID); getErr == nil {
		t.Fatal("publication persisted despite transaction rollback")
	}
	decision.Verdict = "approve"
	if err = st.AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if err = st.AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if count, countErr := st.CountEvents(ctx, atomicTaskID, "review.completed"); countErr != nil || count != 1 {
		t.Fatalf("review.completed after retry=%d err=%v", count, countErr)
	}
	if current, getErr := st.GetTask(ctx, atomicTaskID); getErr != nil || current.State != core.TaskApproved {
		t.Fatalf("atomic task after acceptance=%+v err=%v", current, getErr)
	}
	if stored, getErr := st.GetReviewPublication(ctx, atomicJob.ID); getErr != nil || stored.State != core.ReviewPublicationQueued {
		t.Fatalf("atomic publication=%+v err=%v", stored, getErr)
	}
}
