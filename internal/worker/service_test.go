package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestPairingHeartbeatHealthAndAutoClaimLifecycle(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	operatorCtx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	token, pairing, err := service.IssuePairing(operatorCtx, time.Minute)
	if err != nil || token == "" || pairing.TokenHash == token {
		t.Fatalf("pairing=%+v token=%q err=%v", pairing, token, err)
	}
	enrollment, err := service.Enroll(t.Context(), token, "laptop")
	if err != nil || enrollment.Credential == "" || enrollment.Worker.CredentialHash == enrollment.Credential {
		t.Fatalf("enrollment=%+v err=%v", enrollment, err)
	}
	if _, err = service.Enroll(t.Context(), token, "again"); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("pairing reuse err=%v", err)
	}
	if _, _, err = service.Authenticate(t.Context(), enrollment.Credential, "other"); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("cross-workspace auth err=%v", err)
	}
	workerCtx, worker, err := service.Authenticate(t.Context(), enrollment.Credential, "demo")
	if err != nil {
		t.Fatal(err)
	}
	worker, err = service.Heartbeat(workerCtx, worker, []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	if err != nil || !worker.Live(now) {
		t.Fatalf("worker=%+v err=%v", worker, err)
	}
	if available, reason := service.AutoAvailable(workerCtx, cfg); !available {
		t.Fatalf("auto unavailable: %s", reason)
	}

	createOrder := func(taskID string, hold bool) {
		task := core.Task{ID: taskID, Workspace: "demo", Hold: hold, SpecApproval: true, MergeApproval: true, PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
		if err := st.CreateTask(workerCtx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(workerCtx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(workerCtx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	createOrder("auto-task", false)
	createOrder("manual-task", true)
	listed, err := service.ListClaimable(workerCtx, worker)
	if err != nil || len(listed) != 1 || listed[0].Task.ID != "auto-task" || listed[0].HarnessSelection != "enforced" || listed[0].Confinement != "none" || listed[0].Auth != "byoa" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	// Held tasks are rejected at claim time (spec §21.31 change 2), and a
	// cleared hold releases the order back to worker claimability.
	if _, claimErr := service.ClaimForWorker(workerCtx, worker, "manual-task-implement-1", core.WorkOrderClaim{SessionID: "session-held", ClientToken: "held-token"}); claimErr == nil || !strings.Contains(claimErr.Error(), "held") {
		t.Fatalf("held claim err=%v", claimErr)
	}
	if _, holdErr := st.SetTaskHold(workerCtx, "manual-task", false); holdErr != nil {
		t.Fatal(holdErr)
	}
	if relisted, relistErr := service.ListClaimable(workerCtx, worker); relistErr != nil || len(relisted) != 2 {
		t.Fatalf("relisted=%+v err=%v", relisted, relistErr)
	}
	if _, holdErr := st.SetTaskHold(workerCtx, "manual-task", true); holdErr != nil {
		t.Fatal(holdErr)
	}
	claimed, err := service.ClaimForWorker(workerCtx, worker, listed[0].Order.ID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "child-token"})
	if err != nil || claimed.WorkerID != worker.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if DefaultClaimLease != 5*time.Minute || claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt) != DefaultClaimLease {
		t.Fatalf("claim lease=%s default=%s, want 5m", claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt), DefaultClaimLease)
	}
	if DefaultLivenessLease != 15*time.Second {
		t.Fatalf("worker liveness lease changed to %s", DefaultLivenessLease)
	}
	deadline := claimed.ExecutionDeadline
	now = now.Add(10 * time.Second)
	renewed, err := service.Renew(workerCtx, worker, claimed.ID, "session-a")
	if err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	released, err := service.Release(workerCtx, worker, claimed.ID, core.WorkOrderRelease{SessionID: "session-a", Reason: "test exit", Outcome: core.WorkOrderOutcomeReleased})
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.IsZero() || !released.ExecutionStartedAt.IsZero() || !released.RetrySuppressed {
		t.Fatalf("released=%+v err=%v", released, err)
	}

	createOrder("submitted-task", false)
	submittedClaim, err := service.ClaimForWorker(workerCtx, worker, "submitted-task-implement-1", core.WorkOrderClaim{SessionID: "session-b", ClientToken: "submitted-token"})
	if err != nil {
		t.Fatal(err)
	}
	submittedClaim.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(workerCtx, submittedClaim); err != nil {
		t.Fatal(err)
	}
	if submitted, renewErr := service.Renew(workerCtx, worker, submittedClaim.ID, "session-b"); renewErr != nil || submitted.State != core.WorkOrderSubmitted || !submitted.ExecutionDeadline.Equal(submittedClaim.ExecutionDeadline) {
		t.Fatalf("submitted renew=%+v err=%v", submitted, renewErr)
	}
}

func TestListClaimableOrdersByQueueEntryWithReviewPreference(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }, RetryDelay: time.Nanosecond, RetryMaximum: time.Nanosecond}
	worker := core.Worker{ID: "claim-order-worker", Workspace: "demo", Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}, LeaseExpiresAt: now.Add(time.Minute)}

	createOrder := func(id string, stage core.Stage, queueEnteredAt, createdAt time.Time) {
		task := core.Task{ID: id, Workspace: "demo", State: core.TaskRunning, NextStage: stage, CreatedAt: createdAt}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: queueEnteredAt, QueueDeadline: queueEnteredAt.Add(24 * time.Hour), CreatedAt: createdAt}); err != nil {
			t.Fatal(err)
		}
	}

	createOrder("newer-entry", core.StageImplement, now.Add(-time.Hour), now.Add(-3*time.Hour))
	createOrder("older-entry-z", core.StageImplement, now.Add(-2*time.Hour), now)
	createOrder("older-entry-a", core.StageImplement, now.Add(-2*time.Hour), now.Add(time.Hour))
	listed, err := service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("claimable orders = %+v, want 3", listed)
	}
	got := []string{listed[0].Order.ID, listed[1].Order.ID, listed[2].Order.ID}
	want := []string{"older-entry-a-implement-1", "older-entry-z-implement-1", "newer-entry-implement-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO claim order = %v, want %v", got, want)
	}

	claimed, err := service.ClaimForWorker(ctx, worker, "older-entry-a-implement-1", core.WorkOrderClaim{SessionID: "release-session", ClientToken: "release-token"})
	if err != nil {
		t.Fatal(err)
	}
	released, err := service.Release(ctx, worker, claimed.ID, core.WorkOrderRelease{SessionID: "release-session", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "test retry"})
	if err != nil {
		t.Fatal(err)
	}
	if !released.QueueEnteredAt.After(now.Add(-time.Minute)) {
		t.Fatalf("released queue entry was not refreshed: %s", released.QueueEnteredAt)
	}
	listed, err = service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("claimable orders after release = %+v, want 3", listed)
	}
	got = []string{listed[0].Order.ID, listed[1].Order.ID, listed[2].Order.ID}
	want = []string{"older-entry-z-implement-1", "newer-entry-implement-1", "older-entry-a-implement-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claim order after release = %v, want %v", got, want)
	}

	createOrder("old-review", core.StageReview, now.Add(-30*time.Minute), now.Add(2*time.Hour))
	listed, err = service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 4 {
		t.Fatalf("claimable orders with review = %+v, want 4", listed)
	}
	if listed[0].Order.Stage != core.StageReview || listed[0].Order.ID != "old-review-review-1" {
		t.Fatalf("review preference did not override queue age: first=%+v", listed[0].Order)
	}
}

func TestReleaseRefreshesHarnessSnapshotFromCurrentConfig(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "refresh-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "refresh-task-implement-1", TaskID: "refresh-task", Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	pinned := &core.HarnessSnapshot{Name: "claude", Command: []string{"claude", "-p", "{prompt}", "{mcp_config}"}}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "refresh-task-implement-1", TaskID: "refresh-task", JobID: "refresh-task-implement-1", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, RequiredHarness: "claude", RequiredModel: "provider/model", RequiredHarnessConfig: pinned, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	worker := core.Worker{ID: "worker-refresh", Workspace: "demo", Name: "refresh", CredentialHash: "hash", CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, "refresh-task-implement-1", core.WorkOrderClaim{SessionID: "session-r", ClientToken: "token-r", WorkerID: worker.ID, Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Harnesses: []config.Harness{{Name: "claude", Command: []string{"claude", "-p", "{prompt}", "{mcp_config}", "--dangerously-skip-permissions"}}}}, nil
	}}
	released, err := service.Release(ctx, worker, "refresh-task-implement-1", core.WorkOrderRelease{SessionID: "session-r", Reason: "harness exited", Outcome: core.WorkOrderOutcomeChildFailure, FailureDetail: "The model is not supported by this provider"})
	if err != nil {
		t.Fatal(err)
	}
	if released.RequiredHarnessConfig == nil || !strings.Contains(strings.Join(released.RequiredHarnessConfig.Command, " "), "--dangerously-skip-permissions") {
		t.Fatalf("released snapshot = %+v", released.RequiredHarnessConfig)
	}
	failures, err := st.ListHarnessModelFailures(ctx)
	if err != nil || len(failures) != 1 || failures[0].Harness != "claude" || failures[0].Model != "provider/model" {
		t.Fatalf("model failures=%+v err=%v", failures, err)
	}
	setup := config.ExecutionSetup{Name: "observed", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "claude", Model: "provider/model", ModelPolicy: config.ModelPolicyExplicit}}}
	available, reason := service.AutoAvailableForSetup(ctx, &config.Config{}, setup)
	if available || !strings.Contains(reason, "claude / provider/model") {
		t.Fatalf("serviceability available=%v reason=%q", available, reason)
	}
	unrelated := setup
	unrelated.Name = "unrelated"
	unrelated.ExecutionSettings.Implementation.Model = "provider/other"
	if projected, projectionErr := service.ModelFailuresForSetup(ctx, unrelated); projectionErr != nil || len(projected) != 0 {
		t.Fatalf("unrelated model failures=%+v err=%v", projected, projectionErr)
	}
	refreshEvents, err := st.CountEvents(ctx, "refresh-task", "work_order.harness_refreshed")
	if err != nil || refreshEvents != 1 {
		t.Fatalf("harness refresh events = %d err=%v", refreshEvents, err)
	}
}

func TestFailureDetailBoundAndProviderModelRejectionMatcher(t *testing.T) {
	detail := strings.Repeat("x", FailureDetailLimit+100) + " unsupported model "
	bounded := boundedFailureDetail(detail)
	if len(bounded) > FailureDetailLimit || !strings.HasSuffix(bounded, "unsupported model") {
		t.Fatalf("bounded detail length=%d suffix=%q", len(bounded), bounded[len(bounded)-20:])
	}
	for _, fixture := range []struct {
		detail string
		want   bool
	}{
		{"The model is not supported when using this account", true},
		{"unsupported model provider/foo", true},
		{"provider returned status 400", false},
		{"harness exited with status 1", false},
	} {
		if got := providerModelRejection(fixture.detail); got != fixture.want {
			t.Fatalf("providerModelRejection(%q)=%v want %v", fixture.detail, got, fixture.want)
		}
	}
}

func TestTaskAvailabilityReportsHarnessHeartbeatAndQueueContext(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	worker := core.Worker{ID: "worker-status", Workspace: "demo", Name: "status", CredentialHash: "hash", CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), []core.HarnessProbe{{Harness: "claude", Healthy: false, CheckedAt: now}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "claude"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "claude", Execution: config.ExecutionMCP}, "review": {Harness: "claude", Execution: config.ExecutionMCP}}}}
	service := &Service{Store: st, Now: func() time.Time { return now }}
	task := core.Task{ID: "worker-status-task", Workspace: "demo", NextStage: core.StageReview}
	orders := []core.WorkOrder{{ID: "seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.WorkOrderQueued, RequiredHarness: "claude", RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired}}
	status := service.TaskAvailability(ctx, cfg, task, orders)
	if status.Available || status.QueueContext != "interrupted" || status.LastHeartbeatAge != "0s" || len(status.RequiredHarnesses) != 1 || status.RequiredHarnesses[0] != "claude" {
		t.Fatalf("status=%+v", status)
	}
	if _, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), []core.HarnessProbe{{Harness: "claude", Healthy: true, CheckedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if healthy := service.TaskAvailability(ctx, cfg, task, orders); !healthy.Available {
		t.Fatalf("healthy status=%+v", healthy)
	}
}

func TestTaskAvailabilityReturnsEmptyRequiredHarnessesAsArray(t *testing.T) {
	status := (&Service{Store: store.NewMemory()}).TaskAvailability(
		store.WithWorkspace(t.Context(), "demo"),
		&config.Config{Workspace: "demo"},
		core.Task{ID: "unrouted-task", Workspace: "demo"},
		nil,
	)
	if status.RequiredHarnesses == nil || len(status.RequiredHarnesses) != 0 {
		t.Fatalf("required harnesses = %#v, want non-nil empty slice", status.RequiredHarnesses)
	}
}

func TestAutoHealthRequiresEveryRoutedHarnessOnOneLiveWorker(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.Harnesses = append(cfg.Harnesses, config.Harness{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second})
	review := cfg.Routing.Stages["review"]
	review.Harness = "claude"
	cfg.Routing.Stages["review"] = review
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }, Now: func() time.Time { return now }}
	ctx := store.WithWorkspace(t.Context(), "demo")
	for index, probes := range [][]core.HarnessProbe{{{Harness: "codex", Healthy: true}}, {{Harness: "claude", Healthy: true}}} {
		worker := core.Worker{ID: core.NewTaskID(), Workspace: "demo", Name: string(rune('a' + index)), CredentialHash: core.NewTaskID(), LeaseExpiresAt: now.Add(time.Minute), Probes: probes, CreatedAt: now}
		if err := st.CreateWorker(ctx, worker); err != nil {
			t.Fatal(err)
		}
	}
	if available, _ := service.AutoAvailable(ctx, cfg); available {
		t.Fatal("different workers each reporting one routed harness enabled Auto")
	}
}

func TestAutoHealthIsScopedToSelectedSetup(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	harness := func(name string) config.Harness {
		return config.Harness{Name: name, Command: []string{name, "{prompt}", "{mcp_config}"}, ProbeCommand: []string{name, "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	}
	setup := func(name, harnessName string) config.ExecutionSetup {
		return config.ExecutionSetup{Name: name, ExecutionSettings: config.ContextualExecutionSettings{
			ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
			Implementation: config.ImplementationSettings{Harness: harnessName, Model: "model", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionInProcess, TimeoutText: "1h", FallbackModel: "review"},
		}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "review"}}}}
	}
	good, broken := setup("good", "codex"), setup("broken", "claude")
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{harness("codex"), harness("claude")}, Setups: []config.ExecutionSetup{good, broken}, DefaultSetup: good.Name}
	worker := core.Worker{ID: "setup-worker", Workspace: "demo", Name: "setup", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "claude", Healthy: false}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, Now: func() time.Time { return now }}
	if available, reason := service.AutoAvailableForSetup(ctx, cfg, good); !available {
		t.Fatalf("good setup unavailable: %s", reason)
	}
	if available, _ := service.AutoAvailableForSetup(ctx, cfg, broken); available {
		t.Fatal("broken setup became available because an unrelated setup was healthy")
	}
}

func TestAutoDispatchRequiresEveryRoutedHarnessHealthyOnClaimingWorker(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.Harnesses = append(cfg.Harnesses, config.Harness{Name: "reviewer", Command: []string{"reviewer", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"reviewer", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second})
	review := cfg.Routing.Stages["review"]
	review.Harness = "reviewer"
	cfg.Routing.Stages["review"] = review
	ctx := store.WithWorkspace(t.Context(), "demo")
	orders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: orders, ConfigProvider: orders.ConfigProvider, Now: func() time.Time { return now }}
	worker := core.Worker{ID: "partial-worker", Workspace: "demo", Name: "partial", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "reviewer", Healthy: false}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "route-health", Workspace: "demo", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "route-health-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if listed, err := service.ListClaimable(ctx, worker); err != nil || len(listed) != 0 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if _, err := service.ClaimForWorker(ctx, worker, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token"}); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("claim error=%v", err)
	}
}

func workerTestConfig() *config.Config {
	harness := config.Harness{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}, "review": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}}}}
}

func TestHarnessFingerprintCanonicalizesEmptyArguments(t *testing.T) {
	base := config.Harness{Name: "codex", Command: []string{"codex"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}
	withEmpty := base
	withEmpty.ModelArgs = []string{}
	if HarnessFingerprint(base) != HarnessFingerprint(withEmpty) {
		t.Fatal("nil and empty optional arguments produced different harness fingerprints")
	}
	changed := base
	changed.Command = []string{"codex-next"}
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different harness commands produced the same fingerprint")
	}
	changed = base
	changed.MCPTransport = config.MCPTransportTOMLOverride
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different MCP transports produced the same fingerprint")
	}
	changed = base
	changed.MCPAttachment = "conveyor"
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different MCP attachment identities produced the same fingerprint")
	}
	changed = base
	changed.StallTimeoutText = "2m"
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different stall timeouts produced the same fingerprint")
	}
}

func TestLegacyHarnessSnapshotDefaultsToJSONFileTransport(t *testing.T) {
	harness := harnessFromSnapshot(&core.HarnessSnapshot{Name: "legacy", ProbeTimeoutText: "5s"})
	if harness.MCPTransport != config.MCPTransportJSONFile {
		t.Fatalf("legacy transport=%q want=%q", harness.MCPTransport, config.MCPTransportJSONFile)
	}
}

func TestImplementationDispatchUsesCapturedEffortAfterHotReload(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.Harnesses[0].EffortArgs["high"] = []string{"--config", `model_reasoning_effort="low"`}
	cfg.Repos = []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}
	orders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: orders, ConfigProvider: orders.ConfigProvider, Now: func() time.Time { return now }}
	task := core.Task{ID: "implementation-effort", Workspace: "demo", Repo: "app", BaseBranch: "main", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "implementation-effort-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	snapshot := &core.HarnessSnapshot{
		Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"},
		EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, Effort: "high",
		EffortArgv: []string{"--config", `model_reasoning_effort="high"`}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s",
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "gpt", RequiredHarness: "codex", RequiredEffort: "high", RequiredHarnessConfig: snapshot, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	snapshotHarness := harnessFromSnapshot(snapshot)
	worker := core.Worker{ID: "implementation-worker", Workspace: "demo", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Fingerprint: HarnessFingerprint(snapshotHarness), Healthy: true}}, CreatedAt: now}
	listed, err := service.ListClaimable(ctx, worker)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	item := listed[0]
	if item.Effort != "high" || !reflect.DeepEqual(item.EffortArgv, snapshot.EffortArgv) || !reflect.DeepEqual(item.Harness.EffortArgs["high"], snapshot.EffortArgv) || !reflect.DeepEqual(item.Repository, cfg.Repos[0]) {
		t.Fatalf("implementation dispatch recomputed hot-reloaded effort: %+v", item)
	}
}

func TestAdversarialReviewPanelPinsWorkerSeatsAndAggregatesOneBounce(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.MaxBounces = 2
	cfg.Harnesses = append(cfg.Harnesses, config.Harness{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second})
	cfg.Review = config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Effort: "high"}, {Model: "claude-review", Harness: "claude", Effort: "high"}}}
	cfg.Repos = []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main"}}
	task := core.Task{ID: "panel-task", Workspace: "demo", Repo: "app", PolicyVersion: 1, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatalf("review dispatch redelivery: %v", err)
	}
	if persisted, _ := st.ListTaskWorkOrders(ctx, task.ID); len(persisted) != 2 {
		t.Fatalf("dispatch redelivery created duplicate seats: %+v", persisted)
	} else {
		bySeat := map[int]core.WorkOrder{}
		for _, order := range persisted {
			bySeat[order.ReviewSeat] = order
		}
		if bySeat[1].RequiredHarnessConfig == nil || bySeat[1].RequiredHarnessConfig.Command[0] != "codex" || bySeat[1].RequiredEffort != "high" || bySeat[2].RequiredHarnessConfig == nil || bySeat[2].RequiredHarnessConfig.Command[0] != "claude" || bySeat[2].RequiredEffort != "high" {
			t.Fatalf("review seats did not snapshot harness execution: %+v", persisted)
		}
	}
	workOrders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	worker := core.Worker{ID: "worker-panel", Workspace: "demo", Name: "panel worker", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "claude", Healthy: true}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	unhealthy := worker
	unhealthy.Probes = []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "claude", Healthy: false}}
	if blocked, listErr := service.ListClaimable(ctx, unhealthy); listErr != nil || len(blocked) != 0 {
		t.Fatalf("unhealthy panel harness was dispatchable: %+v err=%v", blocked, listErr)
	}
	listed, err := service.ListClaimable(ctx, worker)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	// Simulate a restart after hot reload removes one harness, changes the
	// other's command, and replaces the next round's panel. The existing round
	// must still accept its old probe and dispatch both original definitions.
	codex := cfg.Harnesses[0]
	codex.Command = []string{"codex-next", "{prompt}", "{mcp_config}"}
	cfg.Harnesses = []config.Harness{codex}
	cfg.Review = config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "next-review", Harness: "codex"}}}
	restarted := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	activeHarnesses, err := restarted.ActiveHarnesses(ctx)
	if err != nil || len(activeHarnesses) != 2 {
		t.Fatalf("active harness snapshots=%+v err=%v", activeHarnesses, err)
	}
	activeProbes := make([]core.HarnessProbe, 0, len(activeHarnesses))
	for _, target := range activeHarnesses {
		activeProbes = append(activeProbes, core.HarnessProbe{Harness: target.Harness.Name, Fingerprint: target.Fingerprint, Healthy: true})
	}
	worker, err = restarted.Heartbeat(ctx, worker, activeProbes)
	if err != nil {
		t.Fatalf("snapshotted harness probe after hot reload: %v", err)
	}
	listed, err = restarted.ListClaimable(ctx, worker)
	if err != nil || len(listed) != 2 {
		t.Fatalf("restarted dispatch after hot reload=%+v err=%v", listed, err)
	}
	bySeat := map[int]DispatchOrder{}
	for _, item := range listed {
		bySeat[item.Order.ReviewSeat] = item
	}
	if bySeat[1].Model != "gpt-review" || bySeat[1].Effort != "high" || bySeat[1].Harness.Name != "codex" || bySeat[1].Harness.Command[0] != "codex" || bySeat[2].Model != "claude-review" || bySeat[2].Effort != "high" || bySeat[2].Harness.Name != "claude" || bySeat[2].Harness.Command[0] != "claude" {
		t.Fatalf("seat dispatch=%+v", bySeat)
	}
	first, err := restarted.ClaimForWorker(ctx, worker, bySeat[1].Order.ID, core.WorkOrderClaim{SessionID: "review-session-1", ClientToken: "review-token-1"})
	if err != nil || first.ModelEnforcement != "worker-pinned" || first.Model != "gpt-review" {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	if _, err = restarted.ClaimForWorker(ctx, worker, bySeat[2].Order.ID, core.WorkOrderClaim{SessionID: "review-session-1", ClientToken: "review-token-2"}); err == nil || !strings.Contains(err.Error(), "session independence") {
		t.Fatalf("same-session claim error=%v", err)
	}
	second, err := restarted.ClaimForWorker(ctx, worker, bySeat[2].Order.ID, core.WorkOrderClaim{SessionID: "review-session-2", ClientToken: "review-token-2"})
	if err != nil || second.ModelEnforcement != "worker-pinned" || second.Model != "claude-review" {
		t.Fatalf("second claim=%+v err=%v", second, err)
	}
	firstResult, err := workOrders.SubmitVerdict(ctx, first.ID, first.SessionID, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat one approved", Feedback: "seat one checked the contract"})
	if err != nil || firstResult["round_status"] != "pending" {
		t.Fatalf("first verdict=%+v err=%v", firstResult, err)
	}
	secondResult, err := workOrders.SubmitVerdict(ctx, second.ID, second.SessionID, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "missing edge case", Feedback: "add the concurrency case"})
	if err != nil || secondResult["round_status"] != "completed" {
		t.Fatalf("second verdict=%+v err=%v", secondResult, err)
	}
	updated, _ := st.GetTask(ctx, task.ID)
	if updated.State != core.TaskQueued || updated.NextStage != core.StageImplement {
		t.Fatalf("task after panel=%+v", updated)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	bounces, completed := 0, 0
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			bounces++
		}
		if event.Kind != "review.round_completed" {
			continue
		}
		completed++
		var payload struct {
			Verdict string `json:"verdict"`
			Reviews []struct {
				ModelEnforcement string `json:"model_enforcement"`
			} `json:"reviews"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil || payload.Verdict != "changes_requested" || len(payload.Reviews) != 2 || payload.Reviews[0].ModelEnforcement != "worker-pinned" || payload.Reviews[1].ModelEnforcement != "worker-pinned" {
			t.Fatalf("aggregate payload=%s err=%v", event.Payload, err)
		}
	}
	if bounces != 1 || completed != 1 {
		t.Fatalf("bounces=%d completed=%d events=%+v", bounces, completed, events)
	}
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if len(interventions) != 1 || !strings.Contains(interventions[0].Comment, "seat one checked") || !strings.Contains(interventions[0].Comment, "add the concurrency case") {
		t.Fatalf("interventions=%+v", interventions)
	}
}
