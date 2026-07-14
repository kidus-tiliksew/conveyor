package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
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
}
