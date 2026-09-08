package storetest

import (
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"testing"
	"time"
)

func runRepositoryInstall(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	cfg := *x.Config
	cfg.Repos = append([]config.Repo(nil), cfg.Repos...)
	for _, enabled := range []bool{true, false} {
		current, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		cfg.Repos[0].InstallConveyor = config.InstallSwitch(enabled)
		_, err = st.UpdateWorkspaceConfig(ctx, current.Version, &cfg)
		requireOK(t, err)
		saved, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		runtime, err := st.RuntimeConfig(ctx, x.Config)
		requireOK(t, err)
		if saved.Document.Repos[0].InstallConveyor == nil || saved.Document.Repos[0].InstallEnabled() != enabled || runtime.Repos[0].InstallEnabled() != enabled {
			t.Fatal("install switch did not round trip")
		}
	}
	repo := cfg.Repos[0].Name
	create := func(attempt int) core.Task {
		id := core.NewTaskID()
		return core.Task{ID: id, Workspace: x.Workspace, Source: core.RepositoryRegistrationSource, IntakeKey: store.RepositoryInstallKey(x.Workspace, repo, attempt), RepositoryInstallAttempt: attempt, Title: "Install Conveyor", Body: "Run conveyor repo init", Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	}
	first := create(1)
	requireOK(t, st.CreateTaskWithDependenciesAndContext(ctx, first, nil, store.TaskContextInput{}))
	attempts, err := st.ListRepositoryInstallTasks(ctx, repo)
	requireOK(t, err)
	if len(attempts) != 1 || attempts[0].TaskID != first.ID || attempts[0].Attempt != 1 || attempts[0].State != core.TaskQueued {
		t.Fatalf("mapping=%+v", attempts)
	}
	duplicate := create(1)
	if err := st.CreateTaskWithDependenciesAndContext(ctx, duplicate, nil, store.TaskContextInput{}); err == nil {
		t.Fatal("duplicate attempt accepted")
	}
	if _, err := st.GetTask(ctx, duplicate.ID); err == nil {
		t.Fatal("failed mapping left a task")
	}
	foreign, err := st.ListRepositoryInstallTasks(store.WithWorkspace(ctx, "other"), repo)
	requireOK(t, err)
	if len(foreign) != 0 {
		t.Fatal("mapping leaked workspace")
	}
	events, err := st.ListEvents(ctx, first.ID)
	requireOK(t, err)
	found := false
	for _, event := range events {
		if event.Kind != "task.created" {
			continue
		}
		var payload core.Task
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Source != core.RepositoryRegistrationSource || payload.RepositoryInstallAttempt != 1 {
			t.Fatal("event lost registration provenance")
		}
		projection := store.ProjectLineageEvent(x.Workspace, event)
		if len(projection.Links) != 0 {
			t.Fatal("registration fabricated lineage links")
		}
		found = true
	}
	if !found {
		t.Fatal("missing task creation event")
	}
	_, err = st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "repository registration replay", RequestID: core.NewTaskID()})
	requireOK(t, err)
	_, err = taskops.New(st).Cancel(ctx, core.Intervention{TaskID: first.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel"})
	requireOK(t, err)
	second := create(2)
	requireOK(t, st.CreateTaskWithDependenciesAndContext(ctx, second, nil, store.TaskContextInput{}))
	attempts, err = st.ListRepositoryInstallTasks(ctx, repo)
	requireOK(t, err)
	if len(attempts) != 2 || attempts[0].State != core.TaskClosed || attempts[1].TaskID != second.ID {
		t.Fatalf("retry mapping=%+v", attempts)
	}
}
