package workorder

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
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

func TestSubmittedOwnerObservationAndTelemetryAreLeaseExempt(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "submitted-owner", Workspace: "demo", Repo: "api", BaseBranch: "main", Branch: "conveyor/task-submitted-owner", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "owner-session", ClientToken: "owner-token", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	claimed.LeaseExpiresAt = time.Now().Add(-time.Minute)
	if err = st.UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "handoff.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("handoff"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, Pack: bundle}

	if _, err = service.Get(ctx, job.ID, "owner-session"); err != nil {
		t.Fatalf("get submitted order: %v", err)
	}
	if _, err = service.ReadArtifact(ctx, job.ID, "owner-session", artifact.ID); err != nil {
		t.Fatalf("read submitted artifact: %v", err)
	}
	if _, err = service.Progress(ctx, job.ID, "owner-session", "review pending"); err != nil {
		t.Fatalf("report submitted progress: %v", err)
	}
	if _, err = service.Usage(ctx, job.ID, "owner-session", 100, 25, 0.5); err != nil {
		t.Fatalf("report submitted usage: %v", err)
	}
	if _, err = service.UploadTranscript(ctx, job.ID, "owner-session", "submitted transcript"); err != nil {
		t.Fatalf("upload submitted transcript: %v", err)
	}
	persisted, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || persisted.State != core.WorkOrderSubmitted || persisted.Progress != "review pending" || persisted.TokensIn != 100 || persisted.TokensOut != 25 || persisted.CostUSD != 0.5 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}

	for name, call := range map[string]func() error{
		"get": func() error {
			_, callErr := service.Get(ctx, job.ID, "other-session")
			return callErr
		},
		"read artifact": func() error {
			_, callErr := service.ReadArtifact(ctx, job.ID, "other-session", artifact.ID)
			return callErr
		},
		"progress": func() error {
			_, callErr := service.Progress(ctx, job.ID, "other-session", "wrong")
			return callErr
		},
		"usage": func() error {
			_, callErr := service.Usage(ctx, job.ID, "other-session", 1, 1, 0)
			return callErr
		},
		"transcript": func() error {
			_, callErr := service.UploadTranscript(ctx, job.ID, "other-session", "wrong")
			return callErr
		},
	} {
		if callErr := call(); callErr == nil || !strings.Contains(callErr.Error(), "another session") {
			t.Errorf("%s from another session error=%v", name, callErr)
		}
	}

	for name, call := range map[string]func() error{
		"submit for review": func() error {
			_, callErr := service.SubmitForReview(ctx, job.ID, "owner-session")
			return callErr
		},
		"submit spec": func() error {
			_, callErr := service.SubmitSpec(ctx, job.ID, "owner-session", pipeline.StructuredSpec{})
			return callErr
		},
		"submit review verdict": func() error {
			_, callErr := service.SubmitVerdict(ctx, job.ID, "owner-session", pipeline.Review{})
			return callErr
		},
	} {
		if callErr := call(); callErr == nil || !strings.Contains(callErr.Error(), "not claimed") {
			t.Errorf("%s from submitted state error=%v", name, callErr)
		}
	}
}

func TestSubmitSpecReturnsValidationAndCompletesClaimedOrder(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "submit-spec", Workspace: "demo", Repo: "api", PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "submit-spec-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "spec-session", ClientToken: "secret", Agent: "codex", Model: "gpt-spec", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	d := &dispatch.Dispatcher{Store: st}
	service := &Service{Store: st, Dispatcher: d}
	invalid := pipeline.StructuredSpec{Markdown: "# Missing required sections", Acceptance: []pipeline.AcceptanceCriterion{{ID: "AC-1", Criterion: "Works", Verify: "test"}}}
	if _, err := service.SubmitSpec(ctx, job.ID, "spec-session", invalid); err == nil {
		t.Fatal("invalid spec submission succeeded")
	}
	claimed, _ := st.GetWorkOrder(ctx, job.ID)
	if claimed.State != core.WorkOrderClaimed {
		t.Fatalf("validation changed order state to %s", claimed.State)
	}
	if _, ok, _ := st.GetLatestSpecVersion(ctx, task.ID); ok {
		t.Fatal("validation failure created a spec version")
	}
	valid := pipeline.StructuredSpec{Markdown: "## Intent\n\nShip it.\n\n## Non-goals\n\nNone.", Acceptance: []pipeline.AcceptanceCriterion{{ID: "AC-1", Criterion: "Works", Verify: "test"}}, Decomposition: []pipeline.DecompositionItem{}}
	if _, err := service.SubmitSpec(ctx, job.ID, "spec-session", valid); err != nil {
		t.Fatal(err)
	}
	version, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok || version.Agent != "codex" || version.Model != "gpt-spec" {
		t.Fatalf("version=%+v ok=%t err=%v", version, ok, err)
	}
	completed, _ := st.GetWorkOrder(ctx, job.ID)
	current, _ := st.GetTask(ctx, task.ID)
	if completed.State != core.WorkOrderCompleted || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("order=%+v task=%+v", completed, current)
	}
}

func TestRetryReviewRoundVerifiesPRHeadAndSnapshotsCurrentPanel(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-retry-service", Workspace: "demo", Repo: "app", Branch: "conveyor/task-review-retry-service", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for seat, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderTimedOut} {
		id := task.ID + "-review-1-seat-" + string(rune('1'+seat))
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Time{}
		if state == core.WorkOrderTimedOut {
			deadline = time.Now().Add(-time.Minute)
		}
		if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview, State: core.WorkOrderQueued, ExecutionDeadline: deadline, ReviewRound: 1, ReviewSeat: seat + 1}); err != nil {
			t.Fatal(err)
		}
		if state == core.WorkOrderCompleted {
			claimed, err := st.ClaimWorkOrder(ctx, id, core.WorkOrderClaim{SessionID: id + "-session", ClientToken: "test-token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			claimed.State = core.WorkOrderCompleted
			if err = st.UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
				t.Fatal(err)
			}
		} else if persisted, err := st.GetWorkOrder(ctx, id); err != nil || persisted.State != core.WorkOrderTimedOut {
			t.Fatalf("timed-out order=%+v err=%v", persisted, err)
		}
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "head_sha": "approved-head"})}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", WorkOrderQueueTimeout: time.Hour,
		Repos:   []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Harness: "codex", TimeoutText: "45m"}}},
		Harnesses: []config.Harness{
			{Name: "codex", Command: []string{"current-codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
			{Name: "claude", Command: []string{"current-claude", "{prompt}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-current", Harness: "codex", Effort: "high"}, {Model: "claude-current", Harness: "claude"}}},
	}
	currentHead := "changed-head"
	service := &Service{
		Store:          st,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 7, HeadSHA: currentHead}, nil
		},
	}
	if _, err := service.RetryReviewRound(ctx, task.ID, "retry-head", "reviewer timed out"); !errors.Is(err, store.ErrReviewRetryConflict) || !strings.Contains(err.Error(), "requires implementation handoff") {
		t.Fatalf("changed head error=%v", err)
	}
	if orders, _ := st.ListTaskWorkOrders(ctx, task.ID); len(orders) != 2 {
		t.Fatalf("changed head created orders=%+v", orders)
	}
	currentHead = "approved-head"
	result, err := service.RetryReviewRound(ctx, task.ID, "retry-head", "reviewer timed out")
	if err != nil || result.NewRound != 2 || len(result.WorkOrders) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.WorkOrders[0].RequiredModel != "gpt-current" || result.WorkOrders[0].RequiredEffort != "high" || result.WorkOrders[0].RequiredHarnessConfig.Command[0] != "current-codex" || result.WorkOrders[1].RequiredHarnessConfig.Command[0] != "current-claude" {
		t.Fatalf("current snapshots=%+v", result.WorkOrders)
	}
	duplicate, err := service.RetryReviewRound(ctx, task.ID, "retry-head", "reviewer timed out")
	if err != nil || duplicate.NewRound != 2 || len(duplicate.WorkOrders) != 2 {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
}

type staticAgent struct {
	output string
	input  inprocess.Input
}

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

func (agent *staticAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.input = input
	return inprocess.Result{Output: agent.output, TokensIn: 10, TokensOut: 4}, nil
}

func TestReviewWorkOrderContextUsesMCPCompletionContract(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "mcp-review-context", Workspace: "test", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "mcp-review-context-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "review-token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return &config.Config{}, nil }}
	result, err := service.Get(ctx, job.ID, "review-session")
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(result.RolePrompt), " ")
	for _, required := range []string{"submit_review_verdict", "wait for and observe a successful tool response", "Printing, returning, or describing verdict JSON is not completion"} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("MCP review context is missing %q: %s", required, result.RolePrompt)
		}
	}
	if strings.Contains(result.RolePrompt, "```conveyor:review") {
		t.Fatalf("MCP review context includes the in-process output contract: %s", result.RolePrompt)
	}
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
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ExecutionTimeoutText: "2h", QueueEnteredAt: queuedAt, QueueDeadline: queuedAt.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	claimed, err := service.Claim(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ExecutionStartedAt.IsZero() || claimed.ExecutionDeadline.Sub(claimed.ExecutionStartedAt) != 2*time.Hour {
		t.Fatalf("execution clocks = start %v deadline %v", claimed.ExecutionStartedAt, claimed.ExecutionDeadline)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].StartedAt.IsZero() || jobs[0].StartedAt.Before(queuedAt.Add(time.Hour)) {
		t.Fatalf("jobs = %+v err=%v", jobs, err)
	}
}

func TestExpiredAttemptRequiresRecoveryAndStartsFreshExecutionWindow(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "execution")
	claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "first", ClientToken: "first-token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	firstDeadline := claimed.ExecutionDeadline
	claimed.LeaseExpiresAt = time.Now().Add(-time.Minute)
	if err = st.UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "second", ClientToken: "second-token", Agent: "codex", Model: "gpt", Lease: 30 * time.Minute}); err == nil || !strings.Contains(err.Error(), "operator recovery") {
		t.Fatalf("claim after expiry error = %v", err)
	}
	expired, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || expired.State != core.WorkOrderQueued || !expired.RetrySuppressed || expired.WorkerID != "" || !expired.ExecutionStartedAt.IsZero() || !expired.ExecutionDeadline.IsZero() {
		t.Fatalf("expired = %+v err=%v", expired, err)
	}
	recovered, err := service.Recover(ctx, order.ID, "recover-1")
	if err != nil || !recovered.Claimable || recovered.RetrySuppressed {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	duplicate, err := service.Recover(ctx, order.ID, "recover-1")
	if err != nil || duplicate.RedispatchCount != recovered.RedispatchCount {
		t.Fatalf("duplicate recovery = %+v err=%v", duplicate, err)
	}
	reclaimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "second", ClientToken: "second-token", Agent: "codex", Model: "gpt", Lease: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed.ExecutionDeadline.After(firstDeadline) || !reclaimed.ExecutionStartedAt.After(claimed.ExecutionStartedAt) {
		t.Fatalf("fresh window first=%v/%v second=%v/%v", claimed.ExecutionStartedAt, firstDeadline, reclaimed.ExecutionStartedAt, reclaimed.ExecutionDeadline)
	}
}

func TestOperatorRecoveryRefreezesNamedSetupAndRepinsOrder(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := store.NewMemory()
	old := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old-model", ModelPolicy: config.ModelPolicyExplicit, Effort: "medium", TimeoutText: "1h"}}}
	current := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "new-model", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"}}}
	task := core.Task{ID: "refreeze-task", Workspace: "demo", State: core.TaskRunning, SetupName: "default", SetupContract: old, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "refreeze-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "old-model", RequiredHarness: "codex", RequiredEffort: "medium", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "old"}, Effort: "medium"}, ExecutionTimeoutText: "1h", LastAttemptOutcome: core.WorkOrderOutcomeChildFailure, RetrySuppressed: true, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}
	if err := st.CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Setups: []config.ExecutionSetup{current}, DefaultSetup: "default", Harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "exec", "{prompt}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}}}, Routing: config.Routing{Stages: map[string]config.StageRoute{}}}, nil
	}}
	recovered, err := service.Recover(ctx, order.ID, "recover-refreeze")
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := st.GetTask(ctx, task.ID)
	if recovered.RequiredModel != "new-model" || recovered.RequiredEffort != "high" || recovered.ExecutionTimeoutText != "2h" || recovered.RequiredHarnessConfig == nil || recovered.RequiredHarnessConfig.Command[1] != "exec" || persisted.SetupContract.ExecutionSettings.Implementation.Model != "new-model" {
		t.Fatalf("recovered=%+v setup=%+v", recovered, persisted.SetupContract)
	}
	events, _ := st.CountEvents(ctx, task.ID, "task.setup.refrozen")
	if events != 1 {
		t.Fatalf("refreeze events=%d", events)
	}
}

func TestOperatorRecoveryRetainsFrozenSetupWhenNamedDefinitionIsMissing(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	frozen := config.ExecutionSetup{Name: "removed", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "frozen-model", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "1h"}}}
	task := core.Task{ID: "missing-setup-task", Workspace: "demo", State: core.TaskRunning, SetupName: "removed", SetupContract: frozen, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "missing-setup-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "frozen-model", RequiredHarness: "codex", ExecutionTimeoutText: "1h", QueueEnteredAt: time.Now().Add(-time.Hour), QueueDeadline: time.Now().Add(-time.Minute)}
	if err := st.CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if persisted, err := st.GetWorkOrder(ctx, order.ID); err != nil || persisted.State != core.WorkOrderStale {
		t.Fatalf("stale order=%+v err=%v", persisted, err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
	}}
	recovered, err := service.Recover(ctx, order.ID, "recover-missing")
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.RequiredModel != "frozen-model" {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	events, _ := st.CountEvents(ctx, task.ID, "task.setup.refrozen")
	if events != 0 {
		t.Fatalf("refreeze events=%d", events)
	}
}

func TestInterruptedReviewRecoveryRefreezesSetupAndRepinsSeat(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := store.NewMemory()
	old := config.ExecutionSetup{Name: "default", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "old-review", Harness: "codex", Effort: "medium"}}}}
	current := config.ExecutionSetup{Name: "default", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "new-review", Harness: "codex", Effort: "high"}}}}
	task := core.Task{ID: "review-refreeze-task", Workspace: "demo", State: core.TaskRunning, SetupName: "default", SetupContract: old, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "review-refreeze-seat", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "old-review", RequiredHarness: "codex", RequiredEffort: "medium", LastAttemptOutcome: core.WorkOrderOutcomeChildFailure, RetrySuppressed: true}
	if err := st.CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Setups: []config.ExecutionSetup{current}, DefaultSetup: "default", Harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "exec", "{prompt}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}}}, Routing: config.Routing{Stages: map[string]config.StageRoute{}}}, nil
	}}
	result, err := service.RecoverInterruptedReviewRound(ctx, task.ID, "recover-review-refreeze")
	if err != nil || len(result.RecoveredOrders) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	recovered := result.RecoveredOrders[0]
	persisted, _ := st.GetTask(ctx, task.ID)
	if recovered.RequiredModel != "new-review" || recovered.RequiredEffort != "high" || recovered.RequiredHarnessConfig == nil || recovered.RequiredHarnessConfig.Command[1] != "exec" || persisted.SetupContract.Review.Seats[0].Model != "new-review" {
		t.Fatalf("recovered=%+v setup=%+v", recovered, persisted.SetupContract)
	}
	events, _ := st.CountEvents(ctx, task.ID, "task.setup.refrozen")
	if events != 1 {
		t.Fatalf("refreeze events=%d", events)
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
		t.Fatalf("audit events stale=%d redispatched=%d", staleEvents, redispatchEvents)
	}
}

func TestRedispatchRejectsOrdersOutsideStaleNeverClaimedGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state core.WorkOrderState
	}{
		{name: "claimed", state: core.WorkOrderClaimed},
		{name: "submitted", state: core.WorkOrderSubmitted},
		{name: "timed-out", state: core.WorkOrderTimedOut},
		{name: "previously-claimed-stale", state: core.WorkOrderStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, st, service, order := newLifecycleService(t, "redispatch-reject-"+tc.name)
			claimed, err := st.ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{
				SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.state != core.WorkOrderClaimed {
				claimed.State = tc.state
				command := core.WorkOrderCmdSubmitForReview
				if tc.state == core.WorkOrderTimedOut {
					command = core.WorkOrderCmdTimeout
				} else if tc.state == core.WorkOrderStale {
					command = core.WorkOrderCmdMarkStale
				}
				if err = st.UpdateWorkOrder(ctx, claimed, command); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = service.Redispatch(ctx, order.ID); err == nil {
				t.Fatalf("redispatch unexpectedly accepted %s order", tc.name)
			}
		})
	}
}

func TestRedispatchRefreshesHarnessSnapshotFromCurrentConfig(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "snapshot-refresh")
	order.RequiredHarness = "claude"
	order.RequiredHarnessConfig = &core.HarnessSnapshot{Name: "claude", MCPTransport: config.MCPTransportJSONFile, Command: []string{"claude", "-p", "{prompt}", "{mcp_config}"}}
	order.QueueDeadline = time.Now().Add(-time.Second)
	if err := st.UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	service.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{
			WorkOrderQueueTimeout: config.DefaultWorkOrderQueueTimeout,
			Routing:               config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}},
			Harnesses:             []config.Harness{{Name: "claude", MCPTransport: config.MCPTransportJSONFile, Command: []string{"claude", "-p", "{prompt}", "{mcp_config}", "--dangerously-skip-permissions"}}},
		}, nil
	}
	redispatched, err := service.Redispatch(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redispatched.State != core.WorkOrderQueued || redispatched.RequiredHarnessConfig == nil ||
		!strings.Contains(strings.Join(redispatched.RequiredHarnessConfig.Command, " "), "--dangerously-skip-permissions") {
		t.Fatalf("redispatched = %+v", redispatched)
	}
	refreshEvents, _ := st.CountEvents(ctx, order.TaskID, "work_order.harness_refreshed")
	if refreshEvents != 1 {
		t.Fatalf("harness refresh events = %d", refreshEvents)
	}
}

func TestRedispatchRetainsSnapshotWhenHarnessRemovedOrEffortUnsupported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		effort    string
		harnesses []config.Harness
	}{
		{name: "removed", harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "exec", "{prompt}", "{mcp_config}"}}}},
		{name: "effort-unsupported", effort: "high", harnesses: []config.Harness{{Name: "claude", Command: []string{"claude", "-p", "{prompt}", "{mcp_config}", "--dangerously-skip-permissions"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, st, service, order := newLifecycleService(t, "snapshot-retain-"+tc.name)
			pinned := &core.HarnessSnapshot{Name: "claude", Command: []string{"claude", "-p", "{prompt}", "{mcp_config}"}, Effort: tc.effort}
			if tc.effort != "" {
				pinned.EffortArgs = map[string][]string{tc.effort: {"--effort", tc.effort}}
				pinned.EffortArgv = []string{"--effort", tc.effort}
			}
			order.RequiredHarness = "claude"
			order.RequiredHarnessConfig = pinned
			order.QueueDeadline = time.Now().Add(-time.Second)
			if err := st.UpdateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			service.ConfigProvider = func(context.Context) (*config.Config, error) {
				return &config.Config{
					WorkOrderQueueTimeout: config.DefaultWorkOrderQueueTimeout,
					Routing:               config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}},
					Harnesses:             tc.harnesses,
				}, nil
			}
			redispatched, err := service.Redispatch(ctx, order.ID)
			if err != nil {
				t.Fatal(err)
			}
			if redispatched.RequiredHarnessConfig == nil || strings.Contains(strings.Join(redispatched.RequiredHarnessConfig.Command, " "), "--dangerously-skip-permissions") {
				t.Fatalf("snapshot should be retained, got %+v", redispatched.RequiredHarnessConfig)
			}
			refreshEvents, _ := st.CountEvents(ctx, order.TaskID, "work_order.harness_refreshed")
			if refreshEvents != 0 {
				t.Fatalf("harness refresh events = %d", refreshEvents)
			}
		})
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

func TestOmittedClaimLeaseDefaultsToFiveMinutesAndExpiresToQueued(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "default-claim-lease")
	claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{
		SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt); got != core.DefaultWorkOrderClaimLease {
		t.Fatalf("default claim lease = %s, want %s", got, core.DefaultWorkOrderClaimLease)
	}
	claimed.LeaseExpiresAt = time.Now().Add(-time.Second)
	if err = st.UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	expired, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || expired.State != core.WorkOrderQueued {
		t.Fatalf("expired order = %+v err=%v", expired, err)
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
	agent := &staticAgent{output: "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"all criteria pass\",\"feedback\":\"\"}\n```"}
	dispatcher := dispatch.New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
		return "diff --git a/app.txt b/app.txt\n-v1\n+v2\n", nil
	}
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
	if !strings.Contains(agent.input.Prompt, "```conveyor:review") || strings.Contains(agent.input.Prompt, "submit_review_verdict") {
		t.Fatalf("in-process review prompt has the wrong terminal contract: %s", agent.input.Prompt)
	}
	if !strings.Contains(agent.input.Prompt, "diff --git a/app.txt b/app.txt") {
		t.Fatalf("in-process review prompt is missing the branch diff: %s", agent.input.Prompt)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskApproved {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
}

func TestSubmitForReviewEvidenceGateIsSideEffectFreeAndPropagatesToEveryReviewSeat(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "evidence-task", Workspace: "demo", Repo: "app", Source: "roadmap:phase-5.4",
		Title: "Evidence change", Branch: "conveyor/evidence-task", BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now(),
	}
	otherTask := core.Task{ID: "other-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	for _, candidate := range []core.Task{task, otherTask} {
		if err := st.CreateTask(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", MaxBounces: 2,
		Execution: config.ExecutionPolicy{RequireVerificationEvidence: true},
		Repos:     []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{
			{Model: "reviewer-a"}, {Model: "reviewer-b"},
		}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.UseDurableQueue()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	openCalls := 0
	var prBody string
	service := &Service{
		Store: st, Dispatcher: dispatcher, Pack: bundle,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		OpenPR: func(_ context.Context, _, _, _, _ string, body string) (string, error) {
			openCalls++
			prBody = body
			return "https://github.com/acme/app/pull/54", nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 54, HeadSHA: "abc123"}, nil
		},
	}

	assertRejectedWithoutSideEffects := func() {
		t.Helper()
		if _, submitErr := service.SubmitForReview(ctx, job.ID, "implementer"); submitErr == nil ||
			!strings.Contains(submitErr.Error(), "role=verification_evidence") ||
			!strings.Contains(submitErr.Error(), "screenshot") {
			t.Fatalf("evidence rejection=%v", submitErr)
		}
		order, getErr := st.GetWorkOrder(ctx, job.ID)
		if getErr != nil || order.State != core.WorkOrderClaimed {
			t.Fatalf("implementation order=%+v err=%v", order, getErr)
		}
		current, getErr := st.GetTask(ctx, task.ID)
		if getErr != nil || current.NextStage != core.StageImplement || current.State != core.TaskRunning {
			t.Fatalf("task advanced on rejection: %+v err=%v", current, getErr)
		}
		orders, listErr := st.ListTaskWorkOrders(ctx, task.ID)
		if listErr != nil || len(orders) != 1 || openCalls != 0 {
			t.Fatalf("side effects orders=%+v open_calls=%d err=%v", orders, openCalls, listErr)
		}
		if count, countErr := st.CountEvents(ctx, task.ID, "pull_request.opened"); countErr != nil || count != 0 {
			t.Fatalf("pull_request.opened=%d err=%v", count, countErr)
		}
	}
	assertRejectedWithoutSideEffects()

	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "wrong-role.png", ContentType: "image/png", Role: core.ArtifactRoleTaskContext, TaskID: task.ID,
	}, []byte("wrong role")); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "other.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: otherTask.ID,
	}, []byte("cross task")); err != nil {
		t.Fatal(err)
	}
	assertRejectedWithoutSideEffects()

	evidence, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "exercised UI `proof`.png", ContentType: "image/png; charset=binary",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
		DownloadURL: "https://control-plane.invalid/private?token=secret",
	}, []byte("valid evidence"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.SubmitForReview(ctx, job.ID, "implementer")
	if err != nil || result["await_review"] != true || openCalls != 1 {
		t.Fatalf("submit=%+v open_calls=%d err=%v", result, openCalls, err)
	}
	if strings.Count(prBody, "<!-- conveyor:verification-evidence -->") != 1 ||
		!strings.Contains(prBody, evidence.ID) || !strings.Contains(prBody, "image/png") ||
		strings.Contains(prBody, "control-plane.invalid") || strings.Contains(prBody, "token=secret") {
		t.Fatalf("unsafe or incomplete PR evidence body: %s", prBody)
	}

	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewSeats := 0
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		reviewSeats++
		session := "review-session-" + order.ID
		if _, err = st.ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "review-secret-" + order.ID, Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
		context, getErr := service.Get(ctx, order.ID, session)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(context.VerificationEvidence) != 1 {
			t.Fatalf("seat %s evidence=%+v", order.ID, context.VerificationEvidence)
		}
		reference := context.VerificationEvidence[0]
		if reference.ID != evidence.ID || reference.WorkOrderID != order.ID || reference.ReadTool != "read_artifact" || reference.DownloadURL != "" {
			t.Fatalf("seat %s reference=%+v", order.ID, reference)
		}
	}
	if reviewSeats != 2 {
		t.Fatalf("review seats=%d orders=%+v", reviewSeats, orders)
	}
}

func TestExpiredWorkerSessionsCannotRenewReleaseOrSubmit(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "stale-session", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		id := task.ID + "-" + string(stage)
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: stage, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1}
		if err := st.CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimWorkOrder(ctx, id, core.WorkOrderClaim{SessionID: "expired-" + string(stage), ClientToken: "token-" + string(stage), WorkerID: "worker", ClaimantID: "worker", Lease: time.Nanosecond, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		session := "expired-" + string(stage)
		if _, err := st.RenewWorkerClaim(ctx, id, "worker", session, time.Minute); !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("%s stale renewal err=%v", stage, err)
		}
		if _, err := st.ReleaseWorkerClaim(ctx, id, "worker", core.WorkOrderRelease{SessionID: session}); !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("%s stale release err=%v", stage, err)
		}
		if stage == core.StageImplement {
			if _, err := service.SubmitForReview(ctx, id, session); err == nil {
				t.Fatal("expired implementation session submitted")
			}
		} else if _, err := service.SubmitVerdict(ctx, id, session, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "stale"}); err == nil {
			t.Fatal("expired review session submitted verdict")
		}
	}
}

func TestSubmitForReviewWaitsForIssueAndPassesClosingReference(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "issue-linked", Workspace: "test", Repo: "app", Title: "Linked change", Branch: "conveyor/issue-linked", BaseBranch: "main", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	lifecycle := core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: spec.Version}
	if err = st.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "issue-linked-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.UseDurableQueue()
	opened := 0
	var body string
	service := &Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }, OpenPR: func(_ context.Context, _, _, _, _ string, value string) (string, error) {
		opened++
		body = value
		return "https://github.com/acme/app/pull/9", nil
	}, ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
		return githubtrigger.ReviewTarget{Number: 9, HeadSHA: "abc"}, nil
	}}
	if _, err = service.SubmitForReview(ctx, job.ID, "implementer"); err == nil || !strings.Contains(err.Error(), "retry after publication") || opened != 0 {
		t.Fatalf("pending issue submit err=%v opened=%d", err, opened)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	lifecycle.State = core.GitHubPublicationPublished
	lifecycle.IssueNumber = 42
	lifecycle.IssueURL = "https://github.com/acme/app/issues/42"
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SubmitForReview(ctx, job.ID, "implementer"); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || !strings.Contains(body, "Closes #42") {
		t.Fatalf("opened=%d body=%q", opened, body)
	}
}

func TestSubmitForReviewAdvancesStaleRefreshHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "stale-refresh", Workspace: "test", Repo: "app", Title: "Fix", Branch: "conveyor/stale-refresh", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "conflict-fix-head", config.RefreshReviewDelta, "merge-conflict"); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "stale-refresh-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.UseDurableQueue()
	service := &Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		OpenPR: func(context.Context, string, string, string, string, string) (string, error) {
			return "https://github.com/acme/app/pull/7", nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 7, HeadSHA: "panel-fix-head"}, nil
		}}
	if _, err := service.SubmitForReview(ctx, job.ID, "implementer"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || !updated.ApprovalStale || updated.RefreshBaselineSHA != "approved-head" || updated.RefreshHeadSHA != "panel-fix-head" {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
	if n, countErr := st.CountEvents(ctx, task.ID, "review.refresh_head_advanced"); countErr != nil || n != 1 {
		t.Fatalf("advance events=%d err=%v", n, countErr)
	}
	// The next refresh round must contract the newly pushed head, not the
	// head recorded when the approval went stale (spec §21.30 change 4).
	_, orders, err := dispatch.BuildReviewRound(cfg, updated, cfg.Routing.Stages["review"], 2)
	if err != nil || len(orders) == 0 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	for _, order := range orders {
		if order.ReviewKind != "refresh" || order.BaselineSHA != "approved-head" || order.HeadSHA != "panel-fix-head" {
			t.Fatalf("refresh order contract = %+v", order)
		}
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
	for _, pendingField := range []string{"status", "decision_rule", "seats", "recommended_next_action", "latest_seat_execution_deadline"} {
		if _, ok := result[pendingField]; ok {
			t.Fatalf("terminal result gained pending field %q: %v", pendingField, result)
		}
	}
}

func TestAwaitReviewPendingIncludesLatestRoundSeatProgressWithoutMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "await-progress-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	implement := core.WorkOrder{ID: "await-progress-implement", TaskID: task.ID, JobID: "await-progress-implement", Stage: core.StageImplement}
	if err := st.CreateJob(ctx, core.Job{ID: implement.JobID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, implement); err != nil {
		t.Fatal(err)
	}
	claimedImplement, err := st.ClaimWorkOrder(ctx, implement.ID, core.WorkOrderClaim{SessionID: "await-progress-owner", ClientToken: "secret", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimedImplement.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, claimedImplement, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}

	createReviewOrder := func(id string, round, seat int) {
		t.Helper()
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat}); err != nil {
			t.Fatal(err)
		}
	}
	createReviewOrder("await-progress-old-round", 1, 1)
	createReviewOrder("await-progress-seat-2", 2, 2)
	createReviewOrder("await-progress-seat-1", 2, 1)
	createReviewOrder("await-progress-seat-3", 2, 3)

	seat1, err := st.ClaimWorkOrder(ctx, "await-progress-seat-1", core.WorkOrderClaim{
		SessionID: "review-seat-1", ClientToken: "seat-1-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	seat1.ExecutionDeadline = time.Now().UTC().Add(time.Hour)
	if err = st.UpdateWorkOrder(ctx, seat1); err != nil {
		t.Fatal(err)
	}
	seat3, err := st.ClaimWorkOrder(ctx, "await-progress-seat-3", core.WorkOrderClaim{
		SessionID: "review-seat-3", ClientToken: "seat-3-token", Lease: time.Minute, ExecutionTimeout: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	seat3.ExecutionDeadline = time.Now().UTC().Add(2 * time.Hour)
	seat3.State = core.WorkOrderCompleted
	if err = st.UpdateWorkOrder(ctx, seat3, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}

	before, err := st.ListTaskWorkOrdersSnapshot(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Service{Store: st}).AwaitReview(ctx, implement.ID, "await-progress-owner", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.ListTaskWorkOrdersSnapshot(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("await_review mutated work orders:\nbefore=%+v\nafter=%+v", before, after)
	}
	if result["status"] != "pending" || result["review_round"] != 2 ||
		result["decision_rule"] != "panel of 3, unanimous to pass" ||
		result["recommended_next_action"] != awaitReviewRecommendation {
		t.Fatalf("pending summary = %#v", result)
	}
	seats, ok := result["seats"].([]awaitReviewSeatProgress)
	if !ok || len(seats) != 3 {
		t.Fatalf("pending seats = %#v", result["seats"])
	}
	for index, seat := range seats {
		if seat.Seat != index+1 || seat.LastActivityAt == nil {
			t.Fatalf("seat ordering/activity = %+v", seats)
		}
	}
	if seats[0].State != core.WorkOrderClaimed || seats[0].VerdictSubmitted || seats[0].ExecutionDeadline == nil {
		t.Fatalf("claimed seat = %+v", seats[0])
	}
	if seats[1].State != core.WorkOrderQueued || seats[1].VerdictSubmitted || seats[1].ExecutionDeadline != nil {
		t.Fatalf("queued seat = %+v", seats[1])
	}
	if seats[2].State != core.WorkOrderCompleted || !seats[2].VerdictSubmitted || seats[2].ExecutionDeadline == nil {
		t.Fatalf("completed seat = %+v", seats[2])
	}
	latest, ok := result["latest_seat_execution_deadline"].(*time.Time)
	if !ok || latest == nil || !latest.Equal(*seats[2].ExecutionDeadline) {
		t.Fatalf("latest deadline = %#v, seats=%+v", result["latest_seat_execution_deadline"], seats)
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
