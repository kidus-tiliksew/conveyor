package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestUpdateJobScopesTransitionThroughTaskWorkspaceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "job-transition-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
	}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{
		ID:        core.NewTaskID(),
		Workspace: workspace,
		Repo:      "repo",
		State:     core.TaskRunning,
		NextStage: core.StageTriage,
		CreatedAt: now,
	}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	job := core.Job{
		ID:        task.ID + "-triage",
		TaskID:    task.ID,
		Stage:     core.StageTriage,
		State:     core.JobRunning,
		StartedAt: now,
	}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	job.State = core.JobDone
	job.EndedAt = now.Add(time.Second)
	if err = st.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobDone || !jobs[0].EndedAt.Equal(job.EndedAt) {
		t.Fatalf("updated jobs=%+v", jobs)
	}
}
