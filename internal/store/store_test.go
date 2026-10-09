package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestMemoryMutationsAppendAttributedEvents(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), Actor{ID: "operator-1", Role: core.ActorHuman})
	st := NewMemory()
	task := core.Task{ID: "task-1", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
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

func TestMemoryActivityMarkerPrefersClaimedOrderStage(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "claimed-activity", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "completed-spec", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone, StartedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "claimed-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: "claimed-implement", TaskID: task.ID, JobID: "claimed-implement", Stage: core.StageImplement,
		State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, "claimed-implement", core.WorkOrderClaim{SessionID: "activity-session", ClientToken: "activity-token", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	if err != nil || len(markers) != 1 || markers[0].LatestStage != core.StageImplement {
		t.Fatalf("markers=%+v err=%v", markers, err)
	}
}
