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
	if err := st.UpdateTaskState(ctx, task.ID, core.TaskRunning); err != nil {
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
	expired.ExecutionDeadline = time.Now().Add(-time.Second)
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
	_, content, err := st.GetArtifact(ctx, created.ID)
	if err != nil || string(content) != "brief" {
		t.Fatalf("content=%q err=%v", content, err)
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
			if _, staleErr := st.RenewWorkerClaim(ctx, job.ID, workerID, "session-0", time.Minute); !errors.Is(staleErr, ErrWorkerUnauthorized) {
				t.Fatalf("stale renew error=%v", staleErr)
			}
			if _, staleErr := st.ReleaseWorkerClaim(ctx, job.ID, workerID, core.WorkOrderRelease{SessionID: "session-0", Outcome: core.WorkOrderOutcomeCancelled}); !errors.Is(staleErr, ErrWorkerUnauthorized) {
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
	events, _ := st.CountEvents(ctx, task.ID, "work_order.recovered")
	if events != 1 {
		t.Fatalf("recovery events=%d", events)
	}
}
