package workorder

import (
	"context"
	"encoding/base64"
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

func TestReadArtifactIsBoundToClaimedWorkOrderContext(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	if err := st.CreateFeature(ctx, core.Feature{ID: "feature-a", Name: "Feature A", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []core.Task{
		{ID: "task-a", Workspace: "demo", FeatureID: "feature-a", State: core.TaskRunning, CreatedAt: time.Now()},
		{ID: "task-b", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []core.WorkOrder{
		{ID: "order-a", TaskID: "task-a", JobID: "job-a", Stage: core.StageImplement, State: core.WorkOrderQueued},
		{ID: "order-b", TaskID: "task-b", JobID: "job-b", Stage: core.StageImplement, State: core.WorkOrderQueued},
	} {
		if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: order.TaskID, Stage: order.Stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: order.ID + "-session", ClientToken: "secret", Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	artifactA, err := st.CreateArtifact(ctx, core.Artifact{Name: "a.pdf", ContentType: "application/pdf", TaskID: "task-a"}, []byte("pdf-a"))
	if err != nil {
		t.Fatal(err)
	}
	artifactB, err := st.CreateArtifact(ctx, core.Artifact{Name: "b.pdf", ContentType: "application/pdf", TaskID: "task-b"}, []byte("pdf-b"))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}
	read, err := service.ReadArtifact(ctx, "order-a", "order-a-session", artifactA.ID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(read.Data)
	if err != nil || string(decoded) != "pdf-a" || read.Artifact.TaskID != "task-a" {
		t.Fatalf("read=%+v decoded=%q err=%v", read, decoded, err)
	}
	if _, err = service.ReadArtifact(ctx, "order-a", "order-a-session", artifactB.ID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-task read error=%v", err)
	}
	featureArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "feature.md", ContentType: "text/markdown", FeatureID: "feature-a"}, []byte("feature context"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReadArtifact(ctx, "order-a", "order-a-session", featureArtifact.ID); err != nil {
		t.Fatalf("assigned feature read: %v", err)
	}
	if _, err = service.ReadArtifact(ctx, "order-a", "wrong-session", artifactA.ID); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("wrong-session read error=%v", err)
	}
}

type staticAgent struct{ output string }

type flakyReviewAcceptanceStore struct {
	store.Store
	failures int
}

func (st *flakyReviewAcceptanceStore) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	if st.failures > 0 {
		st.failures--
		return errors.New("review acceptance unavailable")
	}
	return st.Store.AcceptReviewDecision(ctx, decision)
}

func (agent staticAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
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

func TestQueuedTimeDoesNotConsumeExecutionTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "timeout-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "timeout-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	queuedAt := time.Now().Add(-2 * time.Hour)
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: queuedAt, QueueDeadline: queuedAt.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	claimed, err := service.Claim(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ExecutionStartedAt.IsZero() || claimed.ExecutionDeadline.Sub(claimed.ExecutionStartedAt) != time.Hour {
		t.Fatalf("execution clocks = start %v deadline %v", claimed.ExecutionStartedAt, claimed.ExecutionDeadline)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].StartedAt.IsZero() || jobs[0].StartedAt.Before(queuedAt.Add(time.Hour)) {
		t.Fatalf("jobs = %+v err=%v", jobs, err)
	}
}

func TestExecutionDeadlineSurvivesLeaseReclaimAndTimesOut(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "execution")
	claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "first", ClientToken: "first-token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	fixedDeadline := claimed.ExecutionDeadline
	claimed.LeaseExpiresAt = time.Now().Add(-time.Minute)
	if err = st.UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "second", ClientToken: "second-token", Agent: "codex", Model: "gpt", Lease: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed.ExecutionDeadline.Equal(fixedDeadline) {
		t.Fatalf("deadline changed from %v to %v", fixedDeadline, reclaimed.ExecutionDeadline)
	}
	reclaimed.ExecutionDeadline = time.Now().Add(-time.Second)
	if err = st.UpdateWorkOrder(ctx, reclaimed); err != nil {
		t.Fatal(err)
	}
	orders, err := service.List(ctx)
	if err != nil || len(orders) != 1 || orders[0].State != core.WorkOrderTimedOut || orders[0].Claimable {
		t.Fatalf("orders = %+v err=%v", orders, err)
	}
	if _, err = service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "third", ClientToken: "third-token", Agent: "codex", Model: "gpt"}); !errors.Is(err, store.ErrWorkOrderTimedOut) {
		t.Fatalf("timeout claim error = %v", err)
	}
	if _, err = service.Progress(ctx, order.ID, "second", "late progress"); !errors.Is(err, store.ErrWorkOrderTimedOut) {
		t.Fatalf("timeout progress error = %v", err)
	}
}

func TestStaleQueuedOrderIsListedNonClaimableAndRejected(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "stale")
	order.QueueDeadline = time.Now().Add(-time.Second)
	if err := st.UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	orders, err := service.List(ctx)
	if err != nil || len(orders) != 1 || orders[0].State != core.WorkOrderStale || orders[0].Claimable {
		t.Fatalf("orders = %+v err=%v", orders, err)
	}
	if _, err = service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt"}); !errors.Is(err, store.ErrWorkOrderStale) {
		t.Fatalf("stale claim error = %v", err)
	}
}

func TestRedispatchStaleOrderResetsQueueClockAndPreservesAudit(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "redispatch")
	order.QueueDeadline = time.Now().Add(-time.Second)
	if err := st.UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(ctx); err != nil {
		t.Fatal(err)
	}
	redispatched, err := service.Redispatch(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redispatched.State != core.WorkOrderQueued || !redispatched.Claimable || redispatched.RedispatchCount != 1 ||
		redispatched.QueueDeadline.Sub(redispatched.QueueEnteredAt) != config.DefaultWorkOrderQueueTimeout ||
		!redispatched.ExecutionStartedAt.IsZero() || !redispatched.ExecutionDeadline.IsZero() {
		t.Fatalf("redispatched = %+v", redispatched)
	}
	if _, err = service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt"}); err != nil {
		t.Fatal(err)
	}
	staleEvents, _ := st.CountEvents(ctx, order.TaskID, "work_order.stale")
	redispatchEvents, _ := st.CountEvents(ctx, order.TaskID, "work_order.redispatched")
	if staleEvents != 1 || redispatchEvents != 1 {
		t.Fatalf("audit events stale=%d redispatch=%d", staleEvents, redispatchEvents)
	}
}

func newLifecycleService(t *testing.T, id string) (context.Context, store.Store, *Service, core.WorkOrder) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: id + "-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: id + "-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(config.DefaultWorkOrderQueueTimeout)}
	if err := st.CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{WorkOrderQueueTimeout: config.DefaultWorkOrderQueueTimeout, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	stored, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, st, service, stored
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
	order, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "owner", ClientToken: "secret", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	order.LeaseExpiresAt = time.Now().Add(-time.Second)
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

func TestSubmitVerdictAcceptanceFailureRemainsRetryable(t *testing.T) {
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
	flaky := &flakyReviewAcceptanceStore{Store: base, failures: 1}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
	dispatcher := dispatch.New(flaky, cfg, nil)
	dispatcher.UseDurableQueue()
	service := &Service{Store: flaky, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}
	if _, err := service.SubmitVerdict(ctx, job.ID, "review-session", review); err == nil || !strings.Contains(err.Error(), "review acceptance unavailable") {
		t.Fatalf("first verdict error = %v", err)
	}
	order, err := base.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("order after failed queue=%+v err=%v", order, err)
	}
	events, _ := base.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "review.completed" || event.Kind == "review.publication_queued" {
			t.Fatalf("partial review acceptance event persisted: %s", event.Kind)
		}
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
