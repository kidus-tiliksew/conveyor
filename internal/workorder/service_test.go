package workorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type staticAgent struct{ output string }

type flakyPublicationStore struct {
	store.Store
	failures int
}

func (st *flakyPublicationStore) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if st.failures > 0 {
		st.failures--
		return errors.New("publication queue unavailable")
	}
	return st.Store.QueueReviewPublication(ctx, publication)
}

func (agent staticAgent) Run(context.Context, string, string) (inprocess.Result, error) {
	return inprocess.Result{Output: agent.output, TokensIn: 10, TokensOut: 4}, nil
}

func TestUsagePersistsHighReportWithoutGating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: "job", TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, "job", core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	reported, err := service.Usage(ctx, claimed.ID, "session", 100_000_000, 25_000_000, 20_000)
	if err != nil {
		t.Fatalf("usage error = %v", err)
	}
	if reported.CostUSD != 20_000 {
		t.Fatalf("returned cost = %v", reported.CostUSD)
	}
	stored, getErr := st.GetWorkOrder(ctx, claimed.ID)
	if getErr != nil || stored.CostUSD != 20_000 || stored.TokensIn != 100_000_000 || stored.TokensOut != 25_000_000 {
		t.Fatalf("stored = %+v err=%v", stored, getErr)
	}
	if _, err = service.Progress(ctx, claimed.ID, "session", "continuing after high usage"); err != nil {
		t.Fatalf("high usage gated progress: %v", err)
	}
}

func TestWorkOrderTimeoutStillEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "timeout-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "timeout-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().Add(-2 * time.Hour)}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	_, err := service.Claim(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "wall clock exhausted") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestExpiredLeaseReturnsWorkOrderToQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "lease-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "lease-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	_, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Lease: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	queued, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || queued.State != core.WorkOrderQueued {
		t.Fatalf("expired order = %+v err=%v", queued, err)
	}
}

func TestSubmitForReviewReturnsSynchronousInProcessVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task-sync", Workspace: "test", Repo: "app", Title: "Change", Branch: "conveyor/task-sync", BaseBranch: "main", Level: core.L0, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, ModelTier: "implementer", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", Agent: "codex", Model: "implementer", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Model: "reviewer", Execution: config.ExecutionInProcess, Timeout: time.Minute},
	}}}
	dispatcher := dispatch.New(st, cfg, staticAgent{output: "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"all criteria pass\",\"feedback\":\"\"}\n```"})
	dispatcher.Pack = bundle
	service := &Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}

	if _, err = service.Usage(ctx, claimed.ID, "implement-session", 100_000_000, 25_000_000, 20_000); err != nil {
		t.Fatalf("high usage report failed: %v", err)
	}
	result, err := service.SubmitForReview(ctx, claimed.ID, "implement-session")
	if err != nil {
		t.Fatal(err)
	}
	if result["await_review"] != false || result["verdict"] != "approve" {
		t.Fatalf("result = %+v", result)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskApproved {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
}

func TestAwaitReviewSubmittedOrderOwnershipTimeoutAndPostLeaseRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "await-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "await-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	order, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "owner", ClientToken: "secret", Lease: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}
	if _, err = service.AwaitReview(ctx, order.ID, "other", time.Millisecond); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("other session error = %v", err)
	}
	result, err := service.AwaitReview(ctx, order.ID, "owner", time.Millisecond)
	if err != nil || result["status"] != "pending" {
		t.Fatalf("pending result=%v err=%v", result, err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: "review-1", Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"verdict": "changes_requested", "feedback": "fix it"}), At: time.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	result, err = service.AwaitReview(ctx, order.ID, "owner", time.Millisecond)
	if err != nil || result["verdict"] != "changes_requested" {
		t.Fatalf("retry result=%v err=%v", result, err)
	}
}

func TestSubmitVerdictQueueFailureRemainsRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := store.NewMemory()
	task := core.Task{ID: "retry-verdict", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "retry-verdict-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning, ModelTier: "reviewer", StartedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "review-token", Model: "reviewer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyPublicationStore{Store: base, failures: 1}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
	dispatcher := dispatch.New(flaky, cfg, nil)
	dispatcher.UseDurableQueue()
	service := &Service{Store: flaky, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}
	if _, err := service.SubmitVerdict(ctx, job.ID, "review-session", review); err == nil || !strings.Contains(err.Error(), "publication queue unavailable") {
		t.Fatalf("first verdict error = %v", err)
	}
	order, err := base.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("order after failed queue=%+v err=%v", order, err)
	}
	if repaired, reconcileErr := base.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("reconciled=%d err=%v", repaired, reconcileErr)
	}
	if _, err = service.SubmitVerdict(ctx, job.ID, "review-session", review); err != nil {
		t.Fatalf("retry verdict: %v", err)
	}
	order, err = base.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderCompleted {
		t.Fatalf("completed order=%+v err=%v", order, err)
	}
	if publication, getErr := base.GetReviewPublication(ctx, job.ID); getErr != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("publication=%+v err=%v", publication, getErr)
	}
}

func TestWarmSessionBounceClaimsNextOrderReusesPRAndCannotSelfReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "loop-task", Workspace: "test", Repo: "app", Title: "Loop", Level: core.L2, State: core.TaskRunning, NextStage: core.StageImplement, Branch: "conveyor/loop", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	implementJob := core.Job{ID: "loop-task-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, ModelTier: "implementer", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, implementJob); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: implementJob.ID, TaskID: task.ID, JobID: implementJob.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	const implementSession = "warm-implementation-session"
	const implementToken = "warm-implementation-token"
	if _, err := st.ClaimWorkOrder(ctx, implementJob.ID, core.WorkOrderClaim{SessionID: implementSession, ClientToken: implementToken, Agent: "codex", Model: "implementer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
		"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.UseDurableQueue()
	openCalls := 0
	service := &Service{
		Store: st, Dispatcher: dispatcher,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		OpenPR: func(context.Context, string, string, string, string, string) (string, error) {
			openCalls++
			return "https://github.com/acme/app/pull/7", nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 7, URL: "https://github.com/acme/app/pull/7", HeadSHA: "commit-sha"}, nil
		},
	}

	firstSubmit, err := service.SubmitForReview(ctx, implementJob.ID, implementSession)
	if err != nil || firstSubmit["pr_url"] != "https://github.com/acme/app/pull/7" {
		t.Fatalf("first submit=%v err=%v", firstSubmit, err)
	}
	firstOrder, _ := st.GetWorkOrder(ctx, implementJob.ID)
	firstOrder.LeaseExpiresAt = time.Now().Add(-time.Hour)
	if err = st.UpdateWorkOrder(ctx, firstOrder); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	var firstReview core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview && order.State == core.WorkOrderQueued {
			firstReview = order
		}
	}
	if firstReview.ID == "" {
		t.Fatalf("review order missing: %+v", orders)
	}
	if _, err = st.ClaimWorkOrder(ctx, firstReview.ID, core.WorkOrderClaim{SessionID: "independent-review-1", ClientToken: "review-token-1", Agent: "codex", Model: "reviewer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SubmitVerdict(ctx, firstReview.ID, "independent-review-1", pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "add coverage", Feedback: "add the loop test"}); err != nil {
		t.Fatal(err)
	}
	verdict, err := service.AwaitReview(ctx, implementJob.ID, implementSession, time.Millisecond)
	if err != nil || verdict["verdict"] != "changes_requested" {
		t.Fatalf("await verdict=%v err=%v", verdict, err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	var secondImplement core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageImplement && order.State == core.WorkOrderQueued {
			secondImplement = order
		}
	}
	if secondImplement.ID == "" {
		t.Fatalf("follow-up implement order missing: %+v", orders)
	}
	if _, err = st.ClaimWorkOrder(ctx, secondImplement.ID, core.WorkOrderClaim{SessionID: implementSession, ClientToken: implementToken, Agent: "codex", Model: "implementer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	secondSubmit, err := service.SubmitForReview(ctx, secondImplement.ID, implementSession)
	if err != nil || secondSubmit["pr_url"] != firstSubmit["pr_url"] || openCalls != 2 {
		t.Fatalf("second submit=%v first=%v calls=%d err=%v", secondSubmit, firstSubmit, openCalls, err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	var secondReview core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview && order.State == core.WorkOrderQueued {
			secondReview = order
		}
	}
	if secondReview.ID == "" {
		t.Fatalf("second review order missing: %+v", orders)
	}
	if _, err = st.ClaimWorkOrder(ctx, secondReview.ID, core.WorkOrderClaim{SessionID: implementSession, ClientToken: implementToken, Agent: "codex", Model: "reviewer", Lease: time.Minute}); err == nil || !strings.Contains(err.Error(), "self-review forbidden") {
		t.Fatalf("self-review error = %v", err)
	}
}
