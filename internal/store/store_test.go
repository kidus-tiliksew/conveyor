package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestMemoryMutationsAppendAttributedEvents(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), Actor{ID: "operator-1", Role: core.ActorHuman})
	st := NewMemory()
	task := core.Task{ID: "task-1", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionTaskState(ctx, task.ID, core.TaskDispatchStart); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{
		TaskID: task.ID, Action: core.InterventionRedirect, ReasonCode: "spec-wrong", Comment: "clarify scope",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for _, event := range events {
		if event.ActorID != "operator-1" || event.ActorRole != core.ActorHuman {
			t.Fatalf("event actor = %s/%s", event.ActorID, event.ActorRole)
		}
	}
}

func TestMemoryCreateWorkOrderRejectsExplicitNonCreateStates(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	task := core.Task{ID: "create-order-state", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderCancelled, core.WorkOrderStale, "unsupported"} {
		id := job.ID + "-" + string(state)
		err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: state})
		var transitionErr *core.ErrInvalidTransition
		if !errors.As(err, &transitionErr) || transitionErr.Space != core.WorkOrderLifecycle || transitionErr.From != "" || transitionErr.Command != string(core.WorkOrderCmdCreate) {
			t.Fatalf("state %q error = %v", state, err)
		}
		if _, getErr := st.GetWorkOrder(ctx, id); getErr == nil {
			t.Fatalf("state %q was persisted", state)
		}
	}
}

func TestMemoryReviewRequeueRecordsStageAdvance(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	task := core.Task{ID: "review-requeue-command", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, ReviewRound: 1, ReviewSeat: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: job.ID, ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "tests", Summary: "revise", Feedback: "fix it", MaxBounces: 3}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetTask(ctx, task.ID)
	if err != nil || persisted.State != core.TaskQueued || persisted.NextStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", persisted, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"stage.advance"`) {
			return
		}
	}
	t.Fatal("review requeue did not record stage.advance")
}

func TestMemoryGitHubLifecycleOnlyEmitsActivityForOutcomesAndRealRetries(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(context.Background(), "test")
	st := NewMemory()
	task := core.Task{ID: "github-lifecycle-events", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: 1}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("lifecycle ok=%t err=%v", ok, err)
	}
	lifecycle.State = core.GitHubPublicationRetrying
	lifecycle.Attempts = 1
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	lifecycle.CreateState = core.GitHubCreateReconciling
	lifecycle.CreateAttempts = 1
	lifecycle.LastError = "   "
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_retry"); countErr != nil || count != 0 {
		t.Fatalf("state-only retry events=%d err=%v", count, countErr)
	}
	lifecycle.LastError = "GitHub returned 503"
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_retry"); countErr != nil || count != 1 {
		t.Fatalf("real retry events=%d err=%v", count, countErr)
	}
	lifecycle.State = core.GitHubPublicationFailed
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_failed"); countErr != nil || count != 1 {
		t.Fatalf("failed events=%d err=%v", count, countErr)
	}
}

func TestReviewVerdictDiagnosticsDistinguishClaimedExpiredAndReleased(t *testing.T) {
	now := time.Now().UTC()
	claimed := core.WorkOrder{
		ID: "review-claimed", JobID: "job-claimed", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 1, ExecutionStartedAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(time.Minute),
	}
	expiredClaim := core.WorkOrder{
		ID: "review-expired", JobID: "job-expired", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 2, LeaseExpiresAt: now.Add(-time.Minute),
	}
	expired := expiredClaim
	expired.State, expired.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	releasedClaim := core.WorkOrder{
		ID: "review-released", JobID: "job-released", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 3, LeaseExpiresAt: now.Add(-time.Minute),
	}
	released := releasedClaim
	released.State, released.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	terminalClaim := core.WorkOrder{
		ID: "review-terminal", JobID: "job-terminal", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 4, LeaseExpiresAt: now.Add(-time.Minute),
	}
	terminal := terminalClaim
	terminal.State, terminal.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	events := []core.Event{
		{JobID: expired.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(expiredClaim), At: now.Add(-2 * time.Minute)},
		{JobID: released.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(releasedClaim), At: now.Add(-2 * time.Minute)},
		{JobID: released.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]string{"reason": "harness exited without terminal verdict submission"}), At: now.Add(-30 * time.Second)},
		{JobID: terminal.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(terminalClaim), At: now.Add(-2 * time.Minute)},
		{JobID: terminal.JobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]string{"review_work_order_id": terminal.ID}), At: now.Add(-30 * time.Second)},
	}
	diagnostics := ReviewVerdictDiagnostics([]core.WorkOrder{claimed, expired, released, terminal}, events, now)
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if diagnostics[0].WorkOrderID != expired.ID || diagnostics[0].Status != ReviewExpiredWithoutVerdict || diagnostics[0].ReviewSeat != 2 {
		t.Fatalf("expired diagnostic=%+v", diagnostics[0])
	}
	if diagnostics[1].WorkOrderID != claimed.ID || diagnostics[1].Status != ReviewClaimedWithoutVerdict || diagnostics[1].ReviewSeat != 1 {
		t.Fatalf("claimed diagnostic=%+v", diagnostics[1])
	}
}

func TestMemoryWorkOrderRejectsSelfReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{{ID: "implement", TaskID: "task", Stage: core.StageImplement}, {ID: "review", TaskID: "task", Stage: core.StageReview}} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []core.WorkOrder{{ID: "implement", TaskID: "task", JobID: "implement", Stage: core.StageImplement, State: core.WorkOrderQueued}, {ID: "review", TaskID: "task", JobID: "review", Stage: core.StageReview, State: core.WorkOrderQueued}} {
		if err := st.CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	claim := core.WorkOrderClaim{SessionID: "session-a", ClientToken: "token-a", Agent: "codex", Model: "gpt"}
	if _, err := st.ClaimWorkOrder(ctx, "implement", claim); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimWorkOrder(ctx, "review", claim); err == nil || !strings.Contains(err.Error(), "self-review forbidden") {
		t.Fatalf("self review error=%v", err)
	}
	claim.SessionID = "session-b"
	claim.ClientToken = "token-b"
	review, err := st.ClaimWorkOrder(ctx, "review", claim)
	if err != nil {
		t.Fatalf("fresh review claim: %v", err)
	}
	if review.ModelEnforcement != "self-reported" {
		t.Fatalf("manual review enforcement=%q", review.ModelEnforcement)
	}
}

func TestMemoryWorkOrderRequiresLinkedTaskJobAndWorkspace(t *testing.T) {
	t.Parallel()
	st := NewMemory()
	ctxA := WithWorkspace(context.Background(), "alpha")
	ctxB := WithWorkspace(context.Background(), "beta")
	for _, task := range []core.Task{{ID: "task-a", Workspace: "alpha"}, {ID: "task-b", Workspace: "beta"}} {
		ctx := ctxA
		if task.Workspace == "beta" {
			ctx = ctxB
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: "job-" + task.Workspace, TaskID: task.ID, Stage: core.StageImplement}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateWorkOrder(ctxB, core.WorkOrder{ID: "wrong-workspace", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageImplement}); err == nil {
		t.Fatal("cross-workspace work order succeeded")
	}
	if err := st.CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-task", TaskID: "task-a", JobID: "job-beta", Stage: core.StageImplement}); err == nil {
		t.Fatal("work order linked a task to another task's job")
	}
	if err := st.CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-stage", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageReview}); err == nil {
		t.Fatal("work order linked a job at the wrong stage")
	}
	if err := st.CreateWorkOrder(ctxA, core.WorkOrder{ID: "valid", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageImplement}); err != nil {
		t.Fatalf("valid work order: %v", err)
	}
}

func TestMemoryTimedOutWorkOrderRejectsStaleUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	task := core.Task{ID: "timeout-task", State: core.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "timeout-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}); err != nil {
		t.Fatal(err)
	}
	stale, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired := stale
	deadline := time.Now().Add(-time.Second)
	expired.ExecutionDeadline = deadline
	if err = st.UpdateWorkOrder(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetWorkOrder(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	stale.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, stale); !errors.Is(err, ErrWorkOrderTimedOut) {
		t.Fatalf("stale update error = %v", err)
	}
	current, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || current.State != core.WorkOrderTimedOut {
		t.Fatalf("current = %+v, err = %v", current, err)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || !jobs[0].EndedAt.Equal(deadline) {
		t.Fatalf("timeout job=%+v err=%v want ended_at=%s", jobs, err, deadline)
	}
}

func TestMemoryArtifactsAreContentAddressedAndLinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task"}); err != nil {
		t.Fatal(err)
	}
	artifact := core.Artifact{Name: "brief.txt", ContentType: "text/plain", TaskID: "task"}
	created, err := st.CreateArtifact(ctx, artifact, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.CreateArtifact(ctx, artifact, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != again.ID {
		t.Fatalf("dedupe failed")
	}
	if created.Role != core.ArtifactRoleTaskContext || !created.Role.ModelInputEligible() {
		t.Fatalf("default artifact role = %q", created.Role)
	}
	_, content, err := st.GetArtifact(ctx, created.ID)
	if err != nil || string(content) != "brief" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestMemoryTranscriptProvenancePreservesAuditAndContextLinks(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task", Workspace: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "job", TaskID: "task", Stage: core.StageTriage}); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"safe":"audit"}`)
	contextArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "user.json", ContentType: "application/json", TaskID: "task"}, content)
	if err != nil {
		t.Fatal(err)
	}
	auditArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "job-transcript.json", ContentType: "application/json", Role: core.ArtifactRoleGeneratedAudit, TaskID: "task"}, content)
	if err != nil {
		t.Fatal(err)
	}
	if contextArtifact.ID != auditArtifact.ID {
		t.Fatalf("content address changed across roles: %q != %q", contextArtifact.ID, auditArtifact.ID)
	}
	if err = st.UpsertTranscript(ctx, core.Transcript{JobID: "job", URI: "artifact://" + auditArtifact.ID}); err != nil {
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
	if roles[core.ArtifactRoleTaskContext] != 1 || roles[core.ArtifactRoleGeneratedAudit] != 1 {
		t.Fatalf("roles = %+v artifacts=%+v", roles, artifacts)
	}
}

func TestMemoryLegacyTranscriptLinkBecomesGeneratedAudit(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task", Workspace: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "job", TaskID: "task", Stage: core.StageTriage}); err != nil {
		t.Fatal(err)
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy-transcript.json", ContentType: "application/json", TaskID: "task"}, []byte("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertTranscript(ctx, core.Transcript{JobID: "job", URI: "artifact://" + artifact.ID}); err != nil {
		t.Fatal(err)
	}
	artifacts, err := st.ListArtifacts(ctx)
	if err != nil || len(artifacts) != 1 || artifacts[0].Role != core.ArtifactRoleGeneratedAudit || artifacts[0].Role.ModelInputEligible() {
		t.Fatalf("artifacts=%+v err=%v", artifacts, err)
	}
}

func TestMemoryArtifactsScopeIdenticalContentByWorkspace(t *testing.T) {
	t.Parallel()
	st := NewMemory()
	ctxA := WithWorkspace(context.Background(), "workspace-a")
	ctxB := WithWorkspace(context.Background(), "workspace-b")
	if err := st.CreateTask(ctxA, core.Task{ID: "task-a", Workspace: "workspace-a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctxB, core.Task{ID: "task-b", Workspace: "workspace-b"}); err != nil {
		t.Fatal(err)
	}

	content := []byte("shared content")
	artifactA, err := st.CreateArtifact(ctxA, core.Artifact{Name: "a.txt", ContentType: "text/plain", TaskID: "task-a"}, content)
	if err != nil {
		t.Fatal(err)
	}
	artifactAAgain, err := st.CreateArtifact(ctxA, core.Artifact{Name: "a.txt", ContentType: "text/plain", TaskID: "task-a"}, content)
	if err != nil {
		t.Fatal(err)
	}
	artifactB, err := st.CreateArtifact(ctxB, core.Artifact{Name: "b.txt", ContentType: "text/plain", TaskID: "task-b"}, content)
	if err != nil {
		t.Fatal(err)
	}
	if artifactA.ID != artifactAAgain.ID || artifactA.ID != artifactB.ID {
		t.Fatalf("content-addressed IDs differ: %q, %q, %q", artifactA.ID, artifactAAgain.ID, artifactB.ID)
	}

	for _, test := range []struct {
		name      string
		ctx       context.Context
		artifact  core.Artifact
		taskID    string
		workspace string
	}{
		{name: "workspace A", ctx: ctxA, artifact: artifactA, taskID: "task-a", workspace: "workspace-a"},
		{name: "workspace B", ctx: ctxB, artifact: artifactB, taskID: "task-b", workspace: "workspace-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, got, err := st.GetArtifactForContext(test.ctx, test.artifact.ID, test.taskID, "")
			if err != nil || resolved.Workspace != test.workspace || resolved.TaskID != test.taskID || string(got) != string(content) {
				t.Fatalf("resolved=%+v content=%q err=%v", resolved, got, err)
			}
			listed, err := st.ListArtifacts(test.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Workspace != test.workspace || listed[0].TaskID != test.taskID {
				t.Fatalf("workspace artifact list leaked another workspace: %+v", listed)
			}
		})
	}

	if _, _, err := st.GetArtifactForContext(ctxA, artifactB.ID, "task-b", ""); err == nil {
		t.Fatal("workspace A resolved workspace B link")
	}
	if _, _, err := st.GetArtifactForContext(ctxB, artifactA.ID, "task-a", ""); err == nil {
		t.Fatal("workspace B resolved workspace A link")
	}
	if _, _, err := st.GetArtifact(context.Background(), artifactA.ID); err == nil {
		t.Fatal("unscoped read of cross-workspace digest was not rejected as ambiguous")
	}
}

func TestMemoryCreateSpecVersionAlwaysStartsUnapproved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "spec-task", State: core.TaskAwaiting}); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: "spec-task", Content: "# Spec", Approved: true, ApprovedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Approved || !created.ApprovedAt.IsZero() {
		t.Fatalf("created spec bypassed approval gate: %+v", created)
	}
}

func TestMemoryStoreMatchesProductionRelationships(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	for _, task := range []core.Task{
		{ID: "task-a", Branch: "conveyor/shared", State: core.TaskQueued},
		{ID: "task-b", Branch: "conveyor/other", State: core.TaskQueued},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTask(ctx, core.Task{ID: "task-c", Branch: "conveyor/shared"}); err == nil {
		t.Fatal("duplicate task branch succeeded")
	}
	if err := st.CreateJob(ctx, core.Job{ID: "missing-job", TaskID: "missing"}); err == nil {
		t.Fatal("job for missing task succeeded")
	}
	job := core.Job{ID: "job-a", TaskID: "task-a", Stage: core.StageImplement}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: "task-b", JobID: job.ID, Kind: "wrong"}); err == nil {
		t.Fatal("cross-task job event succeeded")
	}
	if err := st.CreateIntervention(ctx, core.Intervention{
		TaskID: "task-b", JobID: job.ID, Action: core.InterventionApprove, ReasonCode: "approved",
	}); err == nil {
		t.Fatal("cross-task intervention succeeded")
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: "task-a", Action: "invalid"}); err == nil {
		t.Fatal("invalid intervention succeeded")
	}
	if err := st.UpsertTranscript(ctx, core.Transcript{JobID: "missing", URI: "missing"}); err == nil {
		t.Fatal("transcript for missing job succeeded")
	}
	if err := st.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "events.jsonl"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Kind != "transcript.persisted" {
		t.Fatalf("transcript event missing: %+v", events)
	}
	if !strings.Contains(string(events[len(events)-1].Payload), "events.jsonl") {
		t.Fatalf("transcript payload = %s", events[len(events)-1].Payload)
	}
	after, err := st.ListEventsAfter(ctx, "task-a", events[len(events)-2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Kind != "transcript.persisted" {
		t.Fatalf("incremental events = %+v", after)
	}
}

func TestMemoryWorkerFailureBackoffSuppressionAndRecovery(t *testing.T) {
	ctx := WithWorkspace(WithActor(context.Background(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	task := core.Task{ID: "retry-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "retry-job", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	workerID := "worker-1"
	policy := core.WorkOrderRelease{Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited: status 1", InitialRetryDelay: time.Second, MaximumRetryDelay: 4 * time.Second, AutomaticRetryLimit: 3}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	var priorStart time.Time
	for attempt := 0; attempt < 4; attempt++ {
		sessionID := fmt.Sprintf("session-%d", attempt)
		claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: sessionID, ClientToken: fmt.Sprintf("token-%d", attempt), ClaimantID: workerID, WorkerID: workerID, Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		if !priorStart.IsZero() && !claimed.ExecutionStartedAt.After(priorStart) {
			t.Fatalf("attempt %d reused execution start %v", attempt, claimed.ExecutionStartedAt)
		}
		priorStart = claimed.ExecutionStartedAt
		if attempt == 1 {
			if _, staleErr := st.RenewWorkerClaim(ctx, job.ID, workerID, "session-0", time.Minute); !errors.Is(staleErr, ErrWorkOrderClaimLost) {
				t.Fatalf("stale renew error=%v", staleErr)
			}
			if _, staleErr := st.ReleaseWorkerClaim(ctx, job.ID, workerID, core.WorkOrderRelease{SessionID: "session-0", Outcome: core.WorkOrderOutcomeCancelled}); !errors.Is(staleErr, ErrWorkOrderClaimLost) {
				t.Fatalf("stale release error=%v", staleErr)
			}
		}
		exit := 1
		policy.SessionID = sessionID
		policy.ExitStatus = &exit
		released, err := st.ReleaseWorkerClaim(ctx, job.ID, workerID, policy)
		if err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		if released.WorkerID != "" || !released.ExecutionStartedAt.IsZero() || !released.ExecutionDeadline.IsZero() {
			t.Fatalf("release %d retained active attempt: %+v", attempt, released)
		}
		if attempt < len(wantDelays) {
			if released.RetrySuppressed || released.AutomaticRetryCount != attempt+1 || released.NextRetryAt.Sub(released.LastFailureAt) != wantDelays[attempt] {
				t.Fatalf("release %d retry state: %+v", attempt, released)
			}
			if _, claimErr := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "too-soon", ClientToken: "too-soon"}); claimErr == nil || !strings.Contains(claimErr.Error(), "backoff") {
				t.Fatalf("release %d immediate claim error=%v", attempt, claimErr)
			}
			released.NextRetryAt = time.Now().Add(-time.Millisecond)
			if err = st.UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		} else if !released.RetrySuppressed || !released.NextRetryAt.IsZero() || released.AutomaticRetryCount != 3 {
			t.Fatalf("final suppression: %+v", released)
		}
	}
	if _, err := st.RecoverWorkOrder(WithWorkspace(ctx, "other"), job.ID, "recover-1", time.Hour); err == nil {
		t.Fatal("cross-workspace recovery succeeded")
	}
	recovered, err := st.RecoverWorkOrder(ctx, job.ID, "recover-1", time.Hour)
	if err != nil || !recovered.Claimable || recovered.RetrySuppressed || recovered.AutomaticRetryCount != 0 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	duplicate, err := st.RecoverWorkOrder(ctx, job.ID, "recover-1", time.Hour)
	if err != nil || duplicate.RedispatchCount != recovered.RedispatchCount {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	events, _ := st.CountEvents(ctx, task.ID, "work_order.redispatched")
	if events != 1 {
		t.Fatalf("redispatch events=%d", events)
	}
}

func TestMemoryStalledOutcomeConsumesRetryAndReachesNeedsOperator(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	task := core.Task{ID: "stalled-retry-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "stalled-retry-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	var released core.WorkOrder
	for attempt := 0; attempt < 4; attempt++ {
		session := fmt.Sprintf("stall-session-%d", attempt)
		if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute}); err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		var err error
		released, err = st.ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{
			SessionID: session, Outcome: core.WorkOrderOutcomeStalled, Reason: "no child output",
			AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond, MaximumRetryDelay: 4 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		if attempt < 3 {
			if released.RetrySuppressed || released.AutomaticRetryCount != attempt+1 {
				t.Fatalf("retry %d state=%+v", attempt, released)
			}
			released.NextRetryAt = time.Now().Add(-time.Millisecond)
			if err = st.UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !released.RetrySuppressed || released.LastAttemptOutcome != core.WorkOrderOutcomeStalled || released.AutomaticRetryCount != 3 {
		t.Fatalf("stalled exhaustion=%+v", released)
	}
	if stalled := StalledTask([]core.WorkOrder{released}); stalled == nil || !strings.Contains(stalled.Reason, "retry") {
		t.Fatalf("needs-operator projection=%+v", stalled)
	}
	if events, err := st.CountEvents(ctx, task.ID, "work_order.stalled"); err != nil || events != 4 {
		t.Fatalf("stalled events=%d err=%v", events, err)
	}
}

func TestMemoryStateMachinesRejectTerminalPublicationAndJobReentry(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	task := core.Task{ID: "terminal-state-guards", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "terminal-job", TaskID: task.ID, State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State = core.JobDone
	if err := st.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State = core.JobRunning
	if err := st.UpdateJob(ctx, job); err == nil {
		t.Fatal("terminal job reentry succeeded")
	}

	if err := st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: 1}); err != nil {
		t.Fatal(err)
	}
	lifecycle, _, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.State = core.GitHubPublicationPublished
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	lifecycle.State = core.GitHubPublicationRetrying
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err == nil {
		t.Fatal("terminal GitHub publication reentry succeeded")
	}

	publication := core.ReviewPublication{ReviewWorkOrderID: "review-publication", TaskID: task.ID, JobID: job.ID}
	if err = st.QueueReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	publication, err = st.GetReviewPublication(ctx, publication.ReviewWorkOrderID)
	if err != nil {
		t.Fatal(err)
	}
	publication.State = core.ReviewPublicationPublished
	if err = st.UpdateReviewPublication(ctx, publication); err == nil {
		t.Fatal("review publication without required comment ID succeeded")
	}
	publication.CommentID = 51
	if err = st.UpdateReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	publication.State = core.ReviewPublicationFailed
	if err = st.UpdateReviewPublication(ctx, publication); err == nil {
		t.Fatal("terminal review publication transition succeeded")
	}
	legacy := core.ReviewPublication{
		ReviewWorkOrderID: "legacy-publication", State: core.ReviewPublicationPublished,
	}
	retry := legacy
	retry.State = core.ReviewPublicationRetrying
	if err = ValidateReviewPublicationUpdate(legacy, retry); err != nil {
		t.Fatalf("legacy missing-comment repair rejected: %v", err)
	}
	legacy.CommentID = 51
	if err = ValidateReviewPublicationUpdate(legacy, retry); err == nil {
		t.Fatal("valid terminal publication was reopened")
	}
}

func TestMemoryCancelTaskIsAtomicAndCancelledSessionIsTerminal(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	task := core.Task{ID: "cancel-task", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "cancel-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	completedJob := core.Job{ID: "completed-order", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, completedJob); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: completedJob.ID, TaskID: task.ID, JobID: completedJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	completedOrder, err := st.ClaimWorkOrder(ctx, completedJob.ID, core.WorkOrderClaim{SessionID: "completed-session", ClientToken: "completed-secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	completedOrder.State = core.WorkOrderCompleted
	if err = st.UpdateWorkOrder(ctx, completedOrder, core.WorkOrderCmdSubmitSpec); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := st.CancelTask(ctx, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	if err != nil || cancelled.State != core.TaskClosed || cancelled.NextStage != "" {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	order, _ := st.GetWorkOrder(ctx, job.ID)
	completed, _ := st.GetWorkOrder(ctx, completedJob.ID)
	if order.State != core.WorkOrderCancelled || order.SessionID != claimed.SessionID || order.LastAttemptOutcome != core.WorkOrderOutcomeCancelled || completed.State != core.WorkOrderCompleted {
		t.Fatalf("orders cancelled=%+v completed=%+v", order, completed)
	}
	if _, err = st.RenewWorkerClaim(ctx, job.ID, "worker", "session", time.Minute); !errors.Is(err, ErrWorkOrderCancelled) {
		t.Fatalf("renew error=%v", err)
	}
	claimed.Progress = "must not land"
	if err = st.UpdateWorkOrder(ctx, claimed); !errors.Is(err, ErrWorkOrderCancelled) {
		t.Fatalf("update error=%v", err)
	}
	interventions, _ := st.ListInterventions(ctx, task.ID)
	cancelEvents, _ := st.CountEvents(ctx, task.ID, "task.cancelled")
	if len(interventions) != 1 || interventions[0].ActorID != "operator" || cancelEvents != 1 {
		t.Fatalf("interventions=%+v events=%d", interventions, cancelEvents)
	}
	if _, err = st.CancelTask(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "again"}); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("second cancel error=%v", err)
	}
	cancelEvents, _ = st.CountEvents(ctx, task.ID, "task.cancelled")
	if cancelEvents != 1 {
		t.Fatalf("duplicate cancellation event count=%d", cancelEvents)
	}
}

func TestStalledTaskDerivesOnlyActionableNonTerminalOrders(t *testing.T) {
	stalled := StalledTask([]core.WorkOrder{{ID: "retry", State: core.WorkOrderQueued, RetrySuppressed: true, LastFailureMessage: "provider rejected model"}})
	if stalled == nil || stalled.WorkOrder.ID != "retry" || stalled.LastFailure == "" {
		t.Fatalf("stalled=%+v", stalled)
	}
	if got := StalledTask([]core.WorkOrder{{ID: "expired", State: core.WorkOrderStale, LastFailureDetail: "queue wait exceeded"}}); got == nil || got.Reason != "queue deadline expired" || got.LastFailure != "queue wait exceeded" {
		t.Fatalf("expired order stalled=%+v", got)
	}
	if got := StalledTask([]core.WorkOrder{{ID: "loop", State: core.WorkOrderQueued, AutomaticRetryCount: 2, LastFailureMessage: "dispatch failed"}}); got == nil || got.Reason != "dispatch is failing repeatedly" {
		t.Fatalf("loop stalled=%+v", got)
	}
	for _, state := range []core.WorkOrderState{
		core.WorkOrderClaimed,
		core.WorkOrderSubmitted,
	} {
		t.Run(string(state), func(t *testing.T) {
			got := StalledTask([]core.WorkOrder{{
				ID:                  string(state),
				State:               state,
				AutomaticRetryCount: 3,
				LastFailureMessage:  "historical dispatch failure",
				LastFailureDetail:   "historical child error",
			}})
			if got != nil {
				t.Fatalf("%s order stalled=%+v", state, got)
			}
		})
	}
	for _, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderCancelled} {
		if got := StalledTask([]core.WorkOrder{{ID: string(state), State: state, RetrySuppressed: true}}); got != nil {
			t.Fatalf("%s order stalled=%+v", state, got)
		}
	}
}

func TestMemoryFirstActivityTimeoutUsesExistingRetryAuditAndStallEvidence(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "first-activity-timeout-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "first-activity-timeout-order", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	const reason = "harness produced no output before first_activity_timeout"
	var released core.WorkOrder
	for attempt := 1; attempt <= 2; attempt++ {
		session := fmt.Sprintf("first-activity-timeout-%d", attempt)
		if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		var err error
		released, err = st.ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{
			SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: reason,
			AutomaticRetryLimit: 1, InitialRetryDelay: time.Millisecond, MaximumRetryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 1 {
			if released.RetrySuppressed || released.NextRetryAt.IsZero() {
				t.Fatalf("first timeout did not enter bounded retry: %+v", released)
			}
			released.NextRetryAt = now.Add(-time.Second)
			if err = st.UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !released.RetrySuppressed || released.LastFailureMessage != reason || released.State != core.WorkOrderQueued {
		t.Fatalf("suppressed timeout=%+v", released)
	}
	stalled := StalledTask([]core.WorkOrder{released})
	if stalled == nil || stalled.LastFailure != reason || stalled.WorkOrder.ID != released.ID {
		t.Fatalf("stalled evidence=%+v", stalled)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	childFailures := 0
	for _, event := range events {
		if event.Kind != "work_order.child_failed" {
			continue
		}
		childFailures++
		if !strings.Contains(string(event.Payload), reason) {
			t.Fatalf("timeout event omitted stable reason: %s", event.Payload)
		}
	}
	if childFailures != 2 || !strings.Contains(string(events[len(events)-1].Payload), `"retry_suppressed":true`) {
		t.Fatalf("child failure events=%d final=%s", childFailures, events[len(events)-1].Payload)
	}
}

func TestMemorySuppressesSecondIdenticalNonEmptyChildFailure(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "identical-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "identical-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		session := fmt.Sprintf("identical-%d", attempt)
		if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		released, err := st.ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "exit status 1", FailureDetail: "  provider rejected model  ", AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 1 {
			if released.RetrySuppressed || released.LastFailureDetail != "provider rejected model" {
				t.Fatalf("first failure=%+v", released)
			}
			released.NextRetryAt = now.Add(-time.Second)
			if err = st.UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !released.RetrySuppressed || !released.NextRetryAt.IsZero() || released.AutomaticRetryCount != 2 || released.RetrySuppressionReason != core.IdenticalFailureSuppressionReason {
			t.Fatalf("second failure=%+v", released)
		}
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if payload := string(events[len(events)-1].Payload); !strings.Contains(payload, `"detail":"provider rejected model"`) || !strings.Contains(payload, core.IdenticalFailureSuppressionReason) {
		t.Fatalf("event payload=%s", payload)
	}

	differentJob := core.Job{ID: "different-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, differentJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: differentJob.ID, TaskID: task.ID, JobID: differentJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	for attempt, detail := range []string{"first output", "different output"} {
		session := fmt.Sprintf("different-%d", attempt)
		if _, err = st.ClaimWorkOrder(ctx, differentJob.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		var released core.WorkOrder
		released, err = st.ReleaseWorkerClaim(ctx, differentJob.ID, "worker", core.WorkOrderRelease{SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, FailureDetail: detail, AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if released.RetrySuppressed || released.NextRetryAt.IsZero() {
			t.Fatalf("different failure %d=%+v", attempt, released)
		}
		if attempt == 0 {
			released.NextRetryAt = time.Now().Add(-time.Second)
			if err = st.UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
}
