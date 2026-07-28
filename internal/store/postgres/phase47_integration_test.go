package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"gopkg.in/yaml.v3"
)

func TestPhase47PersistenceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase47-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, workspace)
	cfg := &config.Config{Workspace: workspace, MaxBounces: 2, Execution: config.ExecutionPolicy{FirstActivityTimeout: 30 * time.Second, FirstActivityTimeoutText: "30s"}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt-5.4", Execution: config.ExecutionInProcess, TimeoutText: "1m", Timeout: time.Minute},
		"spec":      {Model: "gpt-5.4", Execution: config.ExecutionInProcess, TimeoutText: "1m", Timeout: time.Minute},
		"implement": {Model: "operator", Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour},
		"review":    {Model: "operator", Execution: config.ExecutionMCP, TimeoutText: "1h", Timeout: time.Hour},
	}}, Repos: []config.Repo{{Name: "api", URL: "https://example.test/api.git", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	record, err := st.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	document := record.Document
	document.Execution.RequireVerificationEvidence = true
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	next, err := config.ParseWorkspaceDocument(raw, cfg, "verification evidence integration")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := st.UpdateWorkspaceConfig(store.WithActor(ctx, store.Actor{ID: "phase54-test", Role: core.ActorHuman}), record.Version, next)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Document.Execution.RequireVerificationEvidence || !containsString(receipt.Sections, "execution") || receipt.ActorID != "phase54-test" {
		t.Fatalf("verification evidence config receipt=%+v", receipt)
	}
	reloaded, err := st.WorkspaceConfig(ctx)
	if err != nil || !reloaded.Document.Execution.RequireVerificationEvidence || reloaded.Version != record.Version+1 {
		t.Fatalf("reloaded config=%+v err=%v", reloaded, err)
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
	lifecycle := core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/api", SpecVersion: 1, Source: "github:acme/api#42", SourceIssueNumber: 42}
	if err = st.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatalf("idempotent lifecycle retry: %v", err)
	}
	storedLifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok || storedLifecycle.State != core.GitHubPublicationQueued || storedLifecycle.CreateState != core.GitHubCreateNotStarted || storedLifecycle.SourceIssueNumber != 42 {
		t.Fatalf("lifecycle=%+v ok=%t err=%v", storedLifecycle, ok, err)
	}
	storedLifecycle.State = core.GitHubPublicationRetrying
	storedLifecycle.CreateState = core.GitHubCreateReconciling
	storedLifecycle.CreateAttempts = 1
	storedLifecycle.ReconcileMisses = 2
	storedLifecycle.Attempts = 3
	storedLifecycle.ForgeErrorCategory = "forge_status"
	storedLifecycle.LastError = "GitHub returned 503"
	if err = st.UpdateGitHubLifecycle(ctx, storedLifecycle); err != nil {
		t.Fatal(err)
	}
	storedLifecycle, ok, err = st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok || storedLifecycle.CreateAttempts != 1 || storedLifecycle.ReconcileMisses != 2 || storedLifecycle.ForgeErrorCategory != "forge_status" || storedLifecycle.LastError != "GitHub returned 503" {
		t.Fatalf("reconciliation lifecycle=%+v ok=%t err=%v", storedLifecycle, ok, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_retry"); countErr != nil || count != 1 {
		t.Fatalf("real retry events=%d err=%v", count, countErr)
	}
	storedLifecycle.State = core.GitHubPublicationPublished
	storedLifecycle.CreateState = core.GitHubCreateConfirmed
	storedLifecycle.ReconcileMisses = 0
	storedLifecycle.IssueNumber = 42
	storedLifecycle.IssueURL = "https://github.com/acme/api/issues/42"
	storedLifecycle.ForgeErrorCategory = ""
	storedLifecycle.LastError = ""
	if err = st.UpdateGitHubLifecycle(ctx, storedLifecycle); err != nil {
		t.Fatal(err)
	}
	if hydrated, getErr := st.GetTask(ctx, task.ID); getErr != nil || hydrated.GitHub == nil || hydrated.GitHub.IssueNumber != 42 || hydrated.GitHub.CreateState != core.GitHubCreateConfirmed || hydrated.GitHub.CreateAttempts != 1 || hydrated.GitHub.ReconcileMisses != 0 {
		t.Fatalf("hydrated task=%+v err=%v", hydrated, getErr)
	}
	failedTask := task
	failedTask.ID = core.NewTaskID()
	failedTask.IntakeKey = ""
	failedTask.Branch = "conveyor/integration-" + failedTask.ID
	if err = st.CreateTask(ctx, failedTask); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: failedTask.ID, Repository: "acme/api", SpecVersion: 1}); err != nil {
		t.Fatal(err)
	}
	failedLifecycle, failedOK, failedErr := st.GetGitHubLifecycle(ctx, failedTask.ID)
	if failedErr != nil || !failedOK {
		t.Fatalf("failed lifecycle ok=%t err=%v", failedOK, failedErr)
	}
	failedLifecycle.State = core.GitHubPublicationFailed
	failedLifecycle.LastError = "GitHub rejected publication"
	if err = st.UpdateGitHubLifecycle(ctx, failedLifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, failedTask.ID, "github_issue.publication_failed"); countErr != nil || count != 1 {
		t.Fatalf("failed events=%d err=%v", count, countErr)
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
	deadline := time.Now().Add(-time.Second)
	clockClaim.ExecutionDeadline = deadline
	if err = st.UpdateWorkOrder(ctx, clockClaim); err != nil {
		t.Fatal(err)
	}
	if count, clockErr := taskops.New(st).TickOrderClock(ctx, time.Now().UTC()); clockErr != nil || count != 1 {
		t.Fatalf("order clock count=%d err=%v", count, clockErr)
	}
	if timedOut, getErr := st.GetWorkOrder(ctx, clockJob.ID); getErr != nil || timedOut.State != core.WorkOrderTimedOut || timedOut.Claimable {
		t.Fatalf("timed out=%+v err=%v", timedOut, getErr)
	}
	clockJobs, listErr := st.ListJobs(ctx, clockTaskID)
	if listErr != nil || len(clockJobs) != 1 || clockJobs[0].EndedAt.Sub(deadline).Abs() > time.Millisecond {
		t.Fatalf("timed-out job=%+v err=%v want ended_at=%s", clockJobs, listErr, deadline)
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
	publication := core.ReviewPublication{ReviewWorkOrderID: task.ID + "-review", TaskID: task.ID, JobID: task.ID + "-review", Verdict: "approve", ReasonCode: "approved", Summary: "passes", ReviewerModel: "gpt", ReviewerSession: "distinct", SameModelAsImplementer: "true", RequiredEffort: "high"}
	if err = st.QueueReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	storedPublication, err := st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || storedPublication.State != core.ReviewPublicationQueued || storedPublication.RequiredEffort != "high" {
		t.Fatalf("publication=%+v err=%v", storedPublication, err)
	}
	storedPublication.State = core.ReviewPublicationRetrying
	storedPublication.Attempts = 1
	storedPublication.ForgeErrorCategory = "forge_permission"
	storedPublication.LastError = "GitHub denied comment publication"
	if err = st.UpdateReviewPublication(ctx, storedPublication); err != nil {
		t.Fatal(err)
	}
	storedPublication, err = st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil || storedPublication.ForgeErrorCategory != "forge_permission" || storedPublication.LastError != "GitHub denied comment publication" {
		t.Fatalf("retrying publication=%+v err=%v", storedPublication, err)
	}
	storedPublication.State = core.ReviewPublicationPublished
	storedPublication.CheckRunID = 41
	storedPublication.ForgeErrorCategory = ""
	storedPublication.LastError = ""
	if err = st.UpdateReviewPublication(ctx, storedPublication); err == nil {
		t.Fatal("published PostgreSQL review projection accepted a missing required comment")
	}
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestArtifactRolePersistenceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "artifact-role-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, workspace)
	cfg := &config.Config{Workspace: workspace, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt-5.6-terra", Execution: config.ExecutionInProcess, TimeoutText: "1m", Timeout: time.Minute},
	}}, Repos: []config.Repo{{Name: "api", URL: "https://example.test/api.git", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "api", Title: "Artifact roles", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageTriage, CreatedAt: time.Now()}
	task.Branch = "conveyor/artifact-role-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-triage-1", TaskID: task.ID, Stage: core.StageTriage, State: core.JobFailed, StartedAt: time.Now(), EndedAt: time.Now()}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"safe":"shared"}`)
	contextArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "user.json", ContentType: "application/json", TaskID: task.ID}, content)
	if err != nil {
		t.Fatal(err)
	}
	auditArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "transcript.json", ContentType: "application/json", Role: core.ArtifactRoleGeneratedAudit, TaskID: task.ID}, content)
	if err != nil {
		t.Fatal(err)
	}
	if contextArtifact.ID != auditArtifact.ID {
		t.Fatalf("content address changed across roles")
	}
	evidenceArtifact, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "proof.png", ContentType: "IMAGE/PNG; charset=binary",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
	}, []byte("verification proof"))
	if err != nil || evidenceArtifact.ContentType != "image/png" || !evidenceArtifact.EligibleVerificationEvidence() {
		t.Fatalf("evidence=%+v err=%v", evidenceArtifact, err)
	}
	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "wrong.gif", ContentType: "image/gif",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
	}, []byte("gif")); err == nil {
		t.Fatal("unsupported verification evidence was accepted")
	}
	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "too-large.png", ContentType: "image/png",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
	}, make([]byte, core.MaxVerificationScreenshotBytes+1)); err == nil {
		t.Fatal("oversized verification evidence was accepted")
	}
	otherWorkspace := store.WithWorkspace(context.Background(), workspace+"-other")
	if _, err = st.CreateArtifact(otherWorkspace, core.Artifact{
		Name: "cross.png", ContentType: "image/png",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
	}, []byte("cross workspace")); err == nil {
		t.Fatal("cross-workspace verification evidence was accepted")
	}
	if err = st.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "artifact://" + auditArtifact.ID}); err != nil {
		t.Fatal(err)
	}
	artifacts, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[core.ArtifactRole]int{}
	for _, artifact := range artifacts {
		roles[artifact.Role]++
	}
	if roles[core.ArtifactRoleTaskContext] != 1 || roles[core.ArtifactRoleGeneratedAudit] != 1 || roles[core.ArtifactRoleVerificationEvidence] != 1 {
		t.Fatalf("roles=%+v artifacts=%+v", roles, artifacts)
	}

	legacyJob := core.Job{ID: task.ID + "-triage-2", TaskID: task.ID, Stage: core.StageTriage, State: core.JobFailed, StartedAt: time.Now(), EndedAt: time.Now()}
	if err = st.CreateJob(ctx, legacyJob); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy.json", ContentType: "application/json", TaskID: task.ID}, []byte("legacy transcript"))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertTranscript(ctx, core.Transcript{JobID: legacyJob.ID, URI: "artifact://" + legacy.ID}); err != nil {
		t.Fatal(err)
	}
	artifacts, err = st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.ID == legacy.ID && artifact.Role != core.ArtifactRoleGeneratedAudit {
			t.Fatalf("legacy transcript role = %q", artifact.Role)
		}
	}
}
