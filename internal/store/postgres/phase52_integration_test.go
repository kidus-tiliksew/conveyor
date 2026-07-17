package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPhase52ReviewPanelPersistenceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase52-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, MaxBounces: 1, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess},
		"spec":      {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess},
		"implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
		"review":    {Model: "reviewer", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
	}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}, {Model: "claude-review"}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Mode: core.TaskModeAuto, PolicyVersion: 1, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs := []core.Job{
		{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "gpt-review", State: core.JobPending},
		{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, ModelTier: "claude-review", State: core.JobPending},
	}
	orders := []core.WorkOrder{
		{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
		{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "claude-review", RequiredHarness: "claude", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
	}
	if err = st.CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatalf("idempotent review round retry: %v", err)
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, Name: "phase52", CredentialHash: "hash-" + core.NewTaskID(), LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "review-1", ClientToken: "token-1", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "codex", Model: "gpt-review", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || first.ModelEnforcement != "worker-pinned" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = st.ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "review-1", ClientToken: "token-2", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour}); err == nil || !strings.Contains(err.Error(), "session independence") {
		t.Fatalf("same-session claim error=%v", err)
	}
	second, err := st.ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "review-2", ClientToken: "token-2", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || second.ModelEnforcement != "worker-pinned" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	base := core.ReviewDecision{TaskID: task.ID, ReviewRound: 1, RequiredHarness: "codex", ModelEnforcement: "worker-pinned", PolicyVersion: 1, MergeApproval: true, MaxBounces: 1}
	firstDecision := base
	firstDecision.JobID, firstDecision.ReviewWorkOrderID, firstDecision.ReviewSeat = first.JobID, first.ID, 1
	firstDecision.Verdict, firstDecision.ReasonCode, firstDecision.Summary, firstDecision.Feedback, firstDecision.RequiredModel = "approve", "approved", "approved", "seat one feedback", "gpt-review"
	if err = st.AcceptReviewDecision(ctx, firstDecision); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskRunning {
		t.Fatalf("task advanced before all verdicts: %+v", current)
	}
	secondDecision := base
	secondDecision.JobID, secondDecision.ReviewWorkOrderID, secondDecision.ReviewSeat = second.JobID, second.ID, 2
	secondDecision.Verdict, secondDecision.ReasonCode, secondDecision.Summary, secondDecision.Feedback, secondDecision.RequiredModel, secondDecision.RequiredHarness = "changes_requested", "tests", "changes", "seat two feedback", "claude-review", "claude"
	if err = st.AcceptReviewDecision(ctx, secondDecision); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	bounces, rounds := 0, 0
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			bounces++
		}
		if event.Kind == "review.round_completed" {
			rounds++
			var payload struct {
				Verdict string            `json:"verdict"`
				Reviews []json.RawMessage `json:"reviews"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.Verdict != "changes_requested" || len(payload.Reviews) != 2 {
				t.Fatalf("round payload=%s", event.Payload)
			}
		}
	}
	if bounces != 1 || rounds != 1 {
		t.Fatalf("bounces=%d rounds=%d", bounces, rounds)
	}
}
