package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
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
	governance := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "DESIGN-runtime", Version: 4, Content: "Pinned", PinnedAtAttachment: true}}, Decisions: []core.Decision{{ID: "DEC-1", Status: core.DecisionConfirmed, Statement: "Pin authority"}}, PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "DESIGN-runtime", Version: 5, ProposalEventID: 99, OriginTaskID: task.ID}}, ResolutionNotes: []string{"malformed proposal omitted"}}
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
	if reloaded.GovernanceSnapshot == nil || len(reloaded.GovernanceSnapshot.Designs) != 1 || reloaded.GovernanceSnapshot.Designs[0].Version != 4 || !reloaded.GovernanceSnapshot.Designs[0].PinnedAtAttachment || len(reloaded.GovernanceSnapshot.Decisions) != 1 || len(reloaded.GovernanceSnapshot.PendingDesignProposals) != 1 || reloaded.GovernanceSnapshot.PendingDesignProposals[0].ProposalEventID != 99 || len(reloaded.GovernanceSnapshot.ResolutionNotes) != 1 {
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

func TestReviewClaimRefreshesProposalEvidenceWithoutRefreshingPinnedAuthorityIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "review-proposal-refresh-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "app", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	document, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-review-refresh", Title: "Review refresh", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Initial\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, initial.Version); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	createReview := func(id string, round int, governance *core.GovernanceSnapshot) {
		t.Helper()
		job := core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
		if createErr := st.CreateJob(ctx, job); createErr != nil {
			t.Fatal(createErr)
		}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview, ReviewRound: round, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: governance}
		if createErr := storetest.For(st).CreateWorkOrder(ctx, order); createErr != nil {
			t.Fatal(createErr)
		}
	}

	createReview(task.ID+"-review-1", 1, nil)
	firstClaim, err := service.Claim(ctx, task.ID+"-review-1", core.WorkOrderClaim{SessionID: "review-refresh-1", ClientToken: "secret-1", ClaimantID: "reviewer-1", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if firstClaim.GovernanceSnapshot == nil || len(firstClaim.GovernanceSnapshot.Designs) != 1 || firstClaim.GovernanceSnapshot.Designs[0].Version != initial.Version || len(firstClaim.GovernanceSnapshot.PendingDesignProposals) != 0 {
		t.Fatalf("first claim governance=%+v", firstClaim.GovernanceSnapshot)
	}
	proposal, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Proposed\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	createReview(task.ID+"-review-2", 2, nil)
	if _, err = service.Claim(ctx, task.ID+"-review-2", core.WorkOrderClaim{SessionID: "review-refresh-2", ClientToken: "secret-2", ClaimantID: "reviewer-2", Lease: time.Minute}); err == nil || !strings.Contains(err.Error(), "waiting on") {
		t.Fatalf("pending proposal review claim error=%v", err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, proposal.Version, initial.Version); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := service.Claim(ctx, task.ID+"-review-2", core.WorkOrderClaim{SessionID: "review-refresh-2", ClientToken: "secret-2", ClaimantID: "reviewer-2", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if secondClaim.GovernanceSnapshot == nil || len(secondClaim.GovernanceSnapshot.Designs) != 1 || secondClaim.GovernanceSnapshot.Designs[0].Version != proposal.Version || len(secondClaim.GovernanceSnapshot.PendingDesignProposals) != 1 || !secondClaim.GovernanceSnapshot.PendingDesignProposals[0].Confirmed {
		t.Fatalf("confirmed proposal claim=%+v", secondClaim.GovernanceSnapshot)
	}
}
