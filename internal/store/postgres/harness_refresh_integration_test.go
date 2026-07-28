package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Queue re-entry re-resolves pinned harness snapshots (spec §21.32): the
// refresh applies only to unclaimed queued or stale orders, persists the new
// definition, and appends work_order.harness_refreshed.
func TestHarnessSnapshotRefreshIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "snapshot-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	pinned := &core.HarnessSnapshot{Name: "claude", MCPTransport: config.MCPTransportJSONFile, Command: []string{"claude", "-p", "{prompt}", "{mcp_config}"}, StallTimeoutText: "10m"}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, RequiredHarness: "claude", RequiredHarnessConfig: pinned, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	refresh := &core.HarnessSnapshot{Name: "claude", MCPTransport: config.MCPTransportJSONFile, Command: []string{"claude", "-p", "{prompt}", "{mcp_config}", "--dangerously-skip-permissions"}, StallTimeoutText: "2m"}
	refreshed, err := st.RefreshWorkOrderHarnessSnapshot(ctx, job.ID, refresh)
	if err != nil || refreshed.RequiredHarnessConfig == nil ||
		!strings.Contains(strings.Join(refreshed.RequiredHarnessConfig.Command, " "), "--dangerously-skip-permissions") ||
		refreshed.RequiredHarnessConfig.StallTimeoutText != "2m" {
		t.Fatalf("refreshed=%+v err=%v", refreshed, err)
	}
	stored, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || stored.RequiredHarnessConfig == nil ||
		!strings.Contains(strings.Join(stored.RequiredHarnessConfig.Command, " "), "--dangerously-skip-permissions") ||
		stored.RequiredHarnessConfig.StallTimeoutText != "2m" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	refreshEvents, err := st.CountEvents(ctx, task.ID, "work_order.harness_refreshed")
	if err != nil || refreshEvents != 1 {
		t.Fatalf("harness refresh events = %d err=%v", refreshEvents, err)
	}
	if _, err = st.RefreshWorkOrderHarnessSnapshot(ctx, job.ID, &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "exec"}}); err == nil || !strings.Contains(err.Error(), "does not pin harness") {
		t.Fatalf("name mismatch err=%v", err)
	}
	if _, err = st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RefreshWorkOrderHarnessSnapshot(ctx, job.ID, refresh); err == nil || !strings.Contains(err.Error(), "unclaimed queue entry") {
		t.Fatalf("claimed refresh err=%v", err)
	}
}
