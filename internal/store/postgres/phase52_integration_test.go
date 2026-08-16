package postgres

import (
	"context"
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPhase52ReviewPanelPersistenceIntegration(t *testing.T) {
	t.Skip("review persistence is seat shape only under DEC-23")
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
	}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}, {Model: "claude-review", Effort: "high"}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	frozenSetup := config.ExecutionSetup{
		Name: "review-heavy",
		ExecutionSettings: config.ContextualExecutionSettings{
			Implementation: config.ImplementationSettings{Harness: "claude", Model: "claude-implement", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h", FallbackModel: "gpt-review", FallbackHarness: "codex"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Harness: "codex"}, {Model: "claude-review", Harness: "claude", Effort: "high"}}},
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Mode: core.TaskModeAuto, PolicyVersion: 1, SetupName: frozenSetup.Name, SetupContract: frozenSetup, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	jobs := []core.Job{
		{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "gpt-review", State: core.JobPending},
		{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, ModelTier: "claude-review", State: core.JobPending},
	}
	orders := []core.WorkOrder{
		{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", RequiredHarness: "codex", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "{prompt}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
		{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, ReviewKind: "refresh", ReviewScope: "delta", BaselineSHA: "approved-head", HeadSHA: "new-head", RequiredModel: "claude-review", RequiredHarness: "claude", RequiredEffort: "high", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "claude", Command: []string{"claude", "{prompt}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, Effort: "high", ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
	}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		t.Fatalf("idempotent review round retry: %v", err)
	}
	restarted, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if persisted, getErr := restarted.GetWorkOrder(ctx, orders[1].ID); getErr != nil || persisted.RequiredHarnessConfig == nil || persisted.RequiredHarnessConfig.Command[0] != "claude" || persisted.RequiredEffort != "high" || persisted.RequiredHarnessConfig.EffortArgs["high"][1] != "high" || persisted.ReviewKind != "refresh" || persisted.ReviewScope != "delta" || persisted.BaselineSHA != "approved-head" || persisted.HeadSHA != "new-head" {
		t.Fatalf("restarted harness snapshot=%+v err=%v", persisted, getErr)
	}
	if persisted, getErr := restarted.GetTask(ctx, task.ID); getErr != nil || persisted.SetupName != frozenSetup.Name || !reflect.DeepEqual(persisted.SetupContract, frozenSetup) {
		t.Fatalf("restarted setup contract=%+v err=%v", persisted.SetupContract, getErr)
	}
	if err = restarted.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	if created, staleErr := restarted.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "new-head", config.RefreshReviewDelta, "head-changed"); staleErr != nil || !created {
		t.Fatalf("first stale episode created=%v err=%v", created, staleErr)
	}
	if created, staleErr := restarted.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "new-head", config.RefreshReviewDelta, "head-changed"); staleErr != nil || created {
		t.Fatalf("duplicate stale episode created=%v err=%v", created, staleErr)
	}
	if count, countErr := restarted.CountEvents(ctx, task.ID, "approval.stale"); countErr != nil || count != 1 {
		t.Fatalf("approval.stale events=%d err=%v", count, countErr)
	}
	if _, err = restarted.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "newer-head", config.RefreshReviewDelta, "head-changed"); err != nil {
		t.Fatal(err)
	}
	if persisted, getErr := restarted.GetTask(ctx, task.ID); getErr != nil || !persisted.ApprovalStale || persisted.RefreshBaselineSHA != "approved-head" || persisted.RefreshHeadSHA != "newer-head" || persisted.RefreshReviewScope != config.RefreshReviewDelta {
		t.Fatalf("stale approval=%+v err=%v", persisted, getErr)
	}
	if count, countErr := restarted.CountEvents(ctx, task.ID, "approval.stale"); countErr != nil || count != 2 {
		t.Fatalf("new head did not create a new stale episode: events=%d err=%v", count, countErr)
	}
	if err = restarted.AdvanceTaskRefreshHead(ctx, task.ID, "fix-head"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.AdvanceTaskRefreshHead(ctx, task.ID, "fix-head"); err != nil {
		t.Fatalf("idempotent refresh-head advance: %v", err)
	}
	if persisted, getErr := restarted.GetTask(ctx, task.ID); getErr != nil || !persisted.ApprovalStale || persisted.RefreshBaselineSHA != "approved-head" || persisted.RefreshHeadSHA != "fix-head" || persisted.RefreshReviewScope != config.RefreshReviewDelta {
		t.Fatalf("advanced refresh head=%+v err=%v", persisted, getErr)
	}
	if advanced, countErr := restarted.CountEvents(ctx, task.ID, "review.refresh_head_advanced"); countErr != nil || advanced != 1 {
		t.Fatalf("advance events=%d err=%v", advanced, countErr)
	}
	if err = restarted.SkipTaskRefresh(ctx, task.ID, "fix-head", "clean-update"); err != nil {
		t.Fatal(err)
	}
	if persisted, getErr := restarted.GetTask(ctx, task.ID); getErr != nil || persisted.ApprovalStale || persisted.ApprovedHeadSHA != "fix-head" {
		t.Fatalf("skipped refresh=%+v err=%v", persisted, getErr)
	}
	st = restarted
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, Name: "phase52", CredentialHash: "hash-" + core.NewTaskID(), LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	first, err := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "review-1", ClientToken: "token-1", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "codex", Model: "gpt-review", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || first.ModelEnforcement != "worker-pinned" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "review-1", ClientToken: "token-2", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour}); err == nil || !strings.Contains(err.Error(), "session independence") {
		t.Fatalf("same-session claim error=%v", err)
	}
	second, err := storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "review-2", ClientToken: "token-2", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || second.ModelEnforcement != "worker-pinned" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	base := core.ReviewDecision{TaskID: task.ID, ReviewRound: 1, RequiredHarness: "codex", ModelEnforcement: "worker-pinned", PolicyVersion: 1, MergeApproval: true, MaxBounces: 1}
	firstDecision := base
	firstDecision.JobID, firstDecision.ReviewWorkOrderID, firstDecision.ReviewSeat = first.JobID, first.ID, 1
	firstDecision.Verdict, firstDecision.ReasonCode, firstDecision.Summary, firstDecision.Feedback, firstDecision.RequiredModel = "approve", "approved", "approved", "seat one feedback", "gpt-review"
	if err = storetest.For(st).AcceptReviewDecision(ctx, firstDecision); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskRunning {
		t.Fatalf("task advanced before all verdicts: %+v", current)
	}
	secondDecision := base
	secondDecision.JobID, secondDecision.ReviewWorkOrderID, secondDecision.ReviewSeat = second.JobID, second.ID, 2
	secondDecision.Verdict, secondDecision.ReasonCode, secondDecision.Summary, secondDecision.Feedback, secondDecision.RequiredModel, secondDecision.RequiredHarness = "changes_requested", "tests", "changes", "seat two feedback", "claude-review", "claude"
	if err = storetest.For(st).AcceptReviewDecision(ctx, secondDecision); err != nil {
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

func TestRefreshReviewSettlementBindsHeadAndParksNonAdvancingRegressionIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "refresh-binding-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	type expected struct {
		approvedHead  string
		approvalStale bool
		baseline      string
		head          string
		scope         string
		guardEvents   int
	}
	tests := []struct {
		name         string
		reviewedHead string
		want         expected
	}{
		{name: "binds refresh head", reviewedHead: "head-b", want: expected{approvedHead: "head-b"}},
		{name: "parks baseline regression", reviewedHead: "head-a", want: expected{approvedHead: "head-a", approvalStale: true, baseline: "head-a", head: "head-b", scope: config.RefreshReviewDelta, guardEvents: 1}},
	}
	type restartExpectation struct {
		id           string
		reviewedHead string
		want         expected
	}
	restartExpectations := make([]restartExpectation, 0, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", PolicyVersion: 1, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
			task.Branch = "conveyor/" + task.ID
			if err = st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			restartExpectations = append(restartExpectations, restartExpectation{id: task.ID, reviewedHead: tt.reviewedHead, want: tt.want})
			if err = st.BindTaskApproval(ctx, task.ID, "head-a"); err != nil {
				t.Fatal(err)
			}
			if _, err = st.MarkTaskApprovalStale(ctx, task.ID, "head-a", "head-b", config.RefreshReviewDelta, "head-changed"); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, ReviewKind: "refresh", ReviewScope: config.RefreshReviewDelta, BaselineSHA: "head-a", HeadSHA: "head-b", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
			if err = storetest.For(st).CreateReviewRound(ctx, task.ID, []core.Job{job}, []core.WorkOrder{order}); err != nil {
				t.Fatal(err)
			}
			claimed, claimErr := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "session-" + task.ID, ClientToken: "token-" + task.ID, Lease: time.Minute, ExecutionTimeout: time.Hour})
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			decision := core.ReviewDecision{
				TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: order.ID, ClaimSession: claimed.SessionID,
				ReviewRound: 1, ReviewSeat: 1, ReviewKind: "refresh", ReviewScope: config.RefreshReviewDelta,
				BaselineSHA: "head-a", HeadSHA: "head-b", ReviewedCommitSHA: tt.reviewedHead,
				Verdict: "approve", ReasonCode: "approved", Summary: tt.name, PolicyVersion: 1, MergeApproval: true,
			}
			if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
				t.Fatal(err)
			}
			persisted, getErr := st.GetTask(ctx, task.ID)
			if getErr != nil || persisted.ReviewedHeadSHA != tt.reviewedHead || persisted.ApprovedHeadSHA != tt.want.approvedHead || persisted.ApprovalStale != tt.want.approvalStale || persisted.RefreshBaselineSHA != tt.want.baseline || persisted.RefreshHeadSHA != tt.want.head || persisted.RefreshReviewScope != tt.want.scope {
				t.Fatalf("settled task=%+v err=%v", persisted, getErr)
			}
			if count, countErr := st.CountEvents(ctx, task.ID, "review.refresh_binding_not_advanced"); countErr != nil || count != tt.want.guardEvents {
				t.Fatalf("guard events=%d err=%v", count, countErr)
			}
		})
	}

	restarted, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	for _, expectation := range restartExpectations {
		persisted, getErr := restarted.GetTask(ctx, expectation.id)
		if getErr != nil || persisted.ReviewedHeadSHA != expectation.reviewedHead || persisted.ApprovedHeadSHA != expectation.want.approvedHead || persisted.ApprovalStale != expectation.want.approvalStale || persisted.RefreshBaselineSHA != expectation.want.baseline || persisted.RefreshHeadSHA != expectation.want.head || persisted.RefreshReviewScope != expectation.want.scope {
			t.Fatalf("restart settlement for %s: task=%+v err=%v", expectation.id, persisted, getErr)
		}
		if count, countErr := restarted.CountEvents(ctx, expectation.id, "review.refresh_binding_not_advanced"); countErr != nil || count != expectation.want.guardEvents {
			t.Fatalf("restart guard events for %s=%d err=%v", expectation.id, count, countErr)
		}
	}
}

func TestPhase52ConcurrentReviewClaimsEnforceIndependenceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase52-claims-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess},
		"spec":      {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess},
		"implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
		"review":    {Model: "reviewer", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
	}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}, {Model: "claude-review"}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, Name: "phase52-claims", CredentialHash: "hash-" + core.NewTaskID(), LeaseExpiresAt: time.Now().UTC().Add(time.Minute), CreatedAt: time.Now().UTC()}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		claims [2]core.WorkOrderClaim
	}{
		{
			name: "duplicate session",
			claims: [2]core.WorkOrderClaim{
				{SessionID: "same-session", ClientToken: "token-1", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "codex", Model: "gpt-review", Lease: time.Minute, ExecutionTimeout: time.Hour},
				{SessionID: "same-session", ClientToken: "token-2", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour},
			},
		},
		{
			name: "duplicate client token",
			claims: [2]core.WorkOrderClaim{
				{SessionID: "session-1", ClientToken: "same-token", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "codex", Model: "gpt-review", Lease: time.Minute, ExecutionTimeout: time.Hour},
				{SessionID: "session-2", ClientToken: "same-token", WorkerID: worker.ID, ClaimantID: worker.ID, Agent: "claude", Model: "claude-review", Lease: time.Minute, ExecutionTimeout: time.Hour},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Mode: core.TaskModeAuto, PolicyVersion: 1, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
			task.Branch = "conveyor/task-" + task.ID
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			jobs := []core.Job{
				{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "gpt-review", State: core.JobPending},
				{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, ModelTier: "claude-review", State: core.JobPending},
			}
			orders := []core.WorkOrder{
				{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
				{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "claude-review", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
			}
			if err := storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
				t.Fatal(err)
			}

			start := make(chan struct{})
			ready := sync.WaitGroup{}
			ready.Add(2)
			results := make(chan error, 2)
			for i := range orders {
				i := i
				go func() {
					ready.Done()
					<-start
					_, claimErr := storetest.For(st).ClaimWorkOrder(ctx, orders[i].ID, tt.claims[i])
					results <- claimErr
				}()
			}
			ready.Wait()
			close(start)
			succeeded, blocked := 0, 0
			for range orders {
				claimErr := <-results
				switch {
				case claimErr == nil:
					succeeded++
				case strings.Contains(claimErr.Error(), "session independence"):
					blocked++
				default:
					t.Fatalf("unexpected concurrent claim error: %v", claimErr)
				}
			}
			if succeeded != 1 || blocked != 1 {
				t.Fatalf("concurrent claims succeeded=%d blocked=%d", succeeded, blocked)
			}
			persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			claimed := 0
			for _, order := range persisted {
				if order.State == core.WorkOrderClaimed {
					claimed++
				}
			}
			if claimed != 1 {
				t.Fatalf("persisted claimed seats=%d orders=%+v", claimed, persisted)
			}
		})
	}
}
