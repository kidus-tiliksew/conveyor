package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPhase56MonitorPersistenceAndWorkspaceIsolationIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase56-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{
		ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Source: "monitor:direct_push",
		IntakeKey: "monitor:direct_push:repo:abc", State: core.TaskQueued,
		NextStage: core.StageTriage, CreatedAt: now,
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	observation := monitor.Observation{
		WorkspaceID: workspace, Repository: "repo", Kind: monitor.DirectPush,
		OccurrenceID: "abc", SourceURL: "https://example.test/commit/abc",
		CommitSHA: "abc", ObservedAt: now,
	}
	if _, fresh, observeErr := st.Observe(ctx, observation); observeErr != nil || !fresh {
		t.Fatalf("fresh=%t err=%v", fresh, observeErr)
	}
	if _, fresh, observeErr := st.Observe(ctx, observation); observeErr != nil || fresh {
		t.Fatalf("duplicate fresh=%t err=%v", fresh, observeErr)
	}
	if _, err = st.LinkTask(ctx, observation.Identity(), task.ID, "created"); err != nil {
		t.Fatal(err)
	}
	if err = st.AuditMonitor(ctx, "monitor.task_created", map[string]any{"task_id": task.ID}); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{
		ID: observation.Identity(), WorkspaceID: workspace, Repository: "repo",
		Kind: monitor.DirectPush, SourceURL: observation.SourceURL, CommitSHA: "abc",
		TaskID: task.ID, DetectedAt: now,
	}
	if _, fresh, err := st.RecordDrift(ctx, drift); err != nil || !fresh {
		t.Fatalf("drift fresh=%t err=%v", fresh, err)
	}
	restarted, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status, err := restarted.MonitorStatus(ctx, true, now.Add(time.Hour))
	if err != nil || status.DriftCount != 1 || len(status.Observations) != 1 ||
		status.Observations[0].DeduplicatedCount != 1 || status.Observations[0].TaskID != task.ID ||
		len(status.Activity) != 1 || status.Activity[0].Kind != "monitor.task_created" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	other := store.WithWorkspace(context.Background(), workspace+"-other")
	if otherStatus, otherErr := restarted.MonitorStatus(other, true, now); otherErr == nil && (otherStatus.DriftCount != 0 || len(otherStatus.Observations) != 0) {
		t.Fatalf("cross-workspace status=%+v", otherStatus)
	}
	if _, err = restarted.ResolveDrift(ctx, drift.ID, "conflict_resolved"); err != nil {
		t.Fatal(err)
	}
	status, err = restarted.MonitorStatus(ctx, true, now.Add(2*time.Hour))
	if err != nil || status.DriftCount != 0 {
		t.Fatalf("resolved status=%+v err=%v", status, err)
	}
}

func TestDriftRequirementAmendmentIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "drift-amendment-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Runtime intent"}, core.RequirementVersion{
		Content: "Runtime behavior remains aligned.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Outside changes require reconciliation."}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + taskID, Title: "Drift task", State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{ID: "drift-" + core.NewTaskID(), WorkspaceID: workspace, Repository: "repo", Kind: monitor.DirectPush,
		SourceURL: "https://example.test/commit/amend", CommitSHA: "amend", RequirementID: requirement.ID, TaskID: task.ID, DetectedAt: time.Now().UTC()}
	if _, fresh, recordErr := st.RecordDrift(ctx, drift); recordErr != nil || !fresh {
		t.Fatalf("fresh=%t err=%v", fresh, recordErr)
	}
	if _, err = st.ResolveDrift(ctx, drift.ID, "requirements_amended"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ResolveDrift(ctx, drift.ID, "requirements_amended"); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListRequirementVersions(ctx, requirement.ID)
	if err != nil || len(versions) != 2 || versions[1].OriginDriftID != drift.ID || versions[1].Confirmed {
		t.Fatalf("versions=%+v err=%v", versions, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconciled := 0
	for _, event := range events {
		if event.Kind == "monitor.drift_reconciled" {
			reconciled++
		}
	}
	if reconciled != 1 {
		t.Fatalf("reconciliation events=%+v", events)
	}
}
