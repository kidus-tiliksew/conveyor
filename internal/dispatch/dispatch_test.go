package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type publicationFlakyStore struct {
	store.Store
	failures int
}

func (st *publicationFlakyStore) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if st.failures > 0 {
		st.failures--
		return errors.New("github publication queue unavailable")
	}
	return st.Store.QueueReviewPublication(ctx, publication)
}

type sequenceAgent struct {
	outputs []string
	next    int
	costUSD float64
}

func (agent *sequenceAgent) Run(context.Context, string, string) (inprocess.Result, error) {
	output := agent.outputs[agent.next]
	agent.next++
	return inprocess.Result{Output: output, TokensIn: 20, TokensOut: 10, CostUSD: agent.costUSD}, nil
}

func TestHighInProcessUsageDoesNotGatePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "high-usage", Workspace: "demo", Repo: "api", Title: "Small fix", Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"chore\",\"automatability\":1,\"route\":\"implement\",\"summary\":\"Ready.\"}\n```"}, costUSD: 20_000}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	job, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || job.State != core.JobDone || job.CostUSD != 20_000 {
		t.Fatalf("job=%+v ok=%t err=%v", job, ok, err)
	}
}

func TestInProcessTriageAndSpecAdvanceToImplementWorkOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "demo", Repo: "api", Title: "Add audit export", Body: "Specify and implement it", BaseBranch: "main", Branch: "conveyor/task-task", Level: core.L2, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{
		"```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.9,\"route\":\"spec\",\"summary\":\"Needs an accepted contract.\"}\n```",
		"# Audit export\n\n## Intent\nAdd the export.\n\n## Non-goals\nNo unrelated formats.\n\n```conveyor:acceptance\n- id: AC-1\n  criterion: Export tests pass\n  verify: test\n  ref: ./...\n```",
	}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"spec":      {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
		"review":    {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle

	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("after spec task=%+v err=%v", current, err)
	}
	spec, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok || spec.AcceptanceCount != 1 || spec.Approved {
		t.Fatalf("spec=%+v ok=%v err=%v", spec, ok, err)
	}
	latest, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("latest job ok=%v err=%v", ok, err)
	}
	if err = dispatcher.HandleIntervention(ctx, current, latest, core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement || orders[0].State != core.WorkOrderQueued {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
}

func TestExternalReviewBounceCreatesNextImplementOrderWithFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "bounce-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, Branch: "conveyor/bounce", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{
		{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone, ModelTier: "impl", StartedAt: time.Now()},
		{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP}}}}
	d := New(st, cfg, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "review"}, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "missing coverage", Feedback: "add the test"}, "review-1", "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	if _, err = st.ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "warm-implement-session", ClientToken: "warm-token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].Comment != "add the test" {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	bounces := 0
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			bounces++
		}
	}
	if err != nil || bounces != 1 {
		t.Fatalf("bounces=%d err=%v", bounces, err)
	}
}

func TestExternalReviewAtBounceCapStopsAtHumanGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "cap-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 1}, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "stop", Feedback: "human help"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskAwaiting || updated.RecoveryStage != core.StageImplement || updated.NextStage != "" {
		t.Fatalf("task=%+v err=%v", updated, err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("unexpected follow-up orders: %+v", orders)
	}
}

func TestPublicationQueueFailurePreservesInternalVerdictAndRouting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := store.NewMemory()
	task := core.Task{ID: "publication-failure", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	flaky := &publicationFlakyStore{Store: base, failures: 1}
	d := New(flaky, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	d.UseDurableQueue()
	err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review")
	if err == nil || !strings.Contains(err.Error(), "publication queue unavailable") {
		t.Fatalf("error = %v", err)
	}
	updated, getErr := base.GetTask(ctx, task.ID)
	if getErr != nil || updated.State != core.TaskApproved {
		t.Fatalf("task=%+v err=%v", updated, getErr)
	}
	if err = d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
		t.Fatalf("recovery retry failed: %v", err)
	}
	publication, err := base.GetReviewPublication(ctx, job.ID)
	if err != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	events, _ := base.ListEvents(ctx, task.ID)
	completed := 0
	for _, event := range events {
		if event.Kind == "review.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("review.completed events = %d, want 1", completed)
	}
}

func TestExternalReviewApprovePreservesLevelRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		level    core.EscalationLevel
		state    core.TaskState
		recovery core.Stage
	}{
		{name: "L0 direct approval", level: core.L0, state: core.TaskApproved},
		{name: "L2 final human gate", level: core.L2, state: core.TaskAwaiting, recovery: core.StageImplement},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			task := core.Task{ID: "approve-" + string(test.level), Workspace: "test", Repo: "app", Level: test.level, State: core.TaskRunning, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2}, nil)
			d.UseDurableQueue()
			if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
				t.Fatal(err)
			}
			updated, err := st.GetTask(ctx, task.ID)
			if err != nil || updated.State != test.state || updated.RecoveryStage != test.recovery {
				t.Fatalf("task=%+v err=%v", updated, err)
			}
		})
	}
}
