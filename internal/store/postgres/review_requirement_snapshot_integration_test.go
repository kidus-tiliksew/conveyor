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
	governance := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "DESIGN-runtime", Version: 4, Content: "Pinned"}}, Decisions: []core.Decision{{ID: "DEC-1", Status: core.DecisionConfirmed, Statement: "Pin authority"}}, PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "DESIGN-runtime", Version: 5, ProposalEventID: 99, OriginTaskID: task.ID}}}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "snapshot-session", ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour, Requirements: snapshot, Governance: governance}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ServedRequirementSnapshot) != 1 || reloaded.ServedRequirementSnapshot[0].Version != 3 || reloaded.ServedRequirementSnapshot[0].Statements[0].AcceptanceCriteria[0].ID != "AC-2.1" {
		t.Fatalf("reloaded snapshot=%+v", reloaded.ServedRequirementSnapshot)
	}
	if reloaded.GovernanceSnapshot == nil || len(reloaded.GovernanceSnapshot.Designs) != 1 || reloaded.GovernanceSnapshot.Designs[0].Version != 4 || len(reloaded.GovernanceSnapshot.Decisions) != 1 || len(reloaded.GovernanceSnapshot.PendingDesignProposals) != 1 || reloaded.GovernanceSnapshot.PendingDesignProposals[0].ProposalEventID != 99 {
		t.Fatalf("reloaded governance snapshot=%+v", reloaded.GovernanceSnapshot)
	}
	emptyJob := core.Job{ID: task.ID + "-review-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err = st.CreateJob(ctx, emptyJob); err != nil {
		t.Fatal(err)
	}
	emptyOrder := core.WorkOrder{ID: emptyJob.ID, TaskID: task.ID, JobID: emptyJob.ID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, emptyOrder); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, emptyOrder.ID, core.WorkOrderClaim{SessionID: "empty-snapshot-session", ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour, Requirements: []core.ServedRequirementContext{}}); err != nil {
		t.Fatal(err)
	}
	emptyReloaded, err := st.GetWorkOrder(ctx, emptyOrder.ID)
	if err != nil || emptyReloaded.ServedRequirementSnapshot == nil || len(emptyReloaded.ServedRequirementSnapshot) != 0 {
		t.Fatalf("reloaded empty snapshot=%+v err=%v", emptyReloaded.ServedRequirementSnapshot, err)
	}
}
