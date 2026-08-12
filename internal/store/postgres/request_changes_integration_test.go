package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestUserRequestChangesCreatesHeldImplementOrderIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "request-changes-" + core.NewTaskID()
	cfg := isolationConfig(workspace)
	ctx := store.WithWorkspace(t.Context(), workspace)
	ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID("requester"), Role: core.ActorUser})
	if seeded, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil || !seeded {
		t.Fatalf("workspace seeded=%t err=%v", seeded, err)
	}
	now := time.Now().UTC()
	task := core.Task{
		ID: "task-" + core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main",
		Branch: "conveyor/request-changes", State: core.TaskRunning, NextStage: core.StageReview,
		RecoveryStage: core.StageImplement, CreatedAt: now,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: now}
	if err := st.CreateJob(ctx, reviewJob); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	feedback := "Line one.\n\n  exact indentation"
	updated, err := taskops.New(st).RequestChanges(ctx, taskops.RequestChanges{
		TaskID: task.ID, JobID: reviewJob.ID, Feedback: feedback, MaxBounces: cfg.MaxBounces, Hold: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != core.TaskQueued || updated.NextStage != core.StageImplement || !updated.Hold {
		t.Fatalf("updated task=%+v", updated)
	}
	var dispatches int
	if err := st.pool.QueryRow(ctx, `
SELECT count(*)
FROM river_job
WHERE kind = 'dispatch_task'
  AND args->>'workspace_id' = $1
  AND args->>'task_id' = $2
  AND state IN ('available', 'pending', 'running', 'retryable', 'scheduled')`, workspace, task.ID).Scan(&dispatches); err != nil || dispatches != 1 {
		t.Fatalf("active dispatches=%d err=%v", dispatches, err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].ActorID != store.UserActorID("requester") || interventions[0].Comment != feedback {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	if err != nil || len(markers) != 1 || !markers[0].UserChangesRequested || !store.TaskNeedsAttention(updated, markers[0], false) {
		t.Fatalf("markers=%+v err=%v", markers, err)
	}
}
