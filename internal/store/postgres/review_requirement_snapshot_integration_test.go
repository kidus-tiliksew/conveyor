package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestReviewRequirementSnapshotSurvivesPostgresReload(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "review-snapshot-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "app", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	snapshot := []core.ServedRequirementContext{{ID: "req-runtime", Title: "Runtime", Version: 3, Statements: []core.RequirementStatement{{ID: "REQ-2", Statement: "Retry safely", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-2.1", Statement: "Retry once"}}}}}}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "snapshot-session", ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour, Requirements: snapshot}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ServedRequirementSnapshot) != 1 || reloaded.ServedRequirementSnapshot[0].Version != 3 || reloaded.ServedRequirementSnapshot[0].Statements[0].AcceptanceCriteria[0].ID != "AC-2.1" {
		t.Fatalf("reloaded snapshot=%+v", reloaded.ServedRequirementSnapshot)
	}
}
