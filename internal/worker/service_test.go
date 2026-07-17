package worker

import (
	"context"
	"encoding/json"
	"errors"
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

	createOrder := func(taskID string, mode core.TaskMode) {
		task := core.Task{ID: taskID, Workspace: "demo", Mode: mode, SpecApproval: true, MergeApproval: true, PolicyVersion: 1, Level: core.LegacyLevel(mode, true, true), State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
		if err := st.CreateTask(workerCtx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(workerCtx, job); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(workerCtx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	createOrder("auto-task", core.TaskModeAuto)
	createOrder("manual-task", core.TaskModeManual)
	listed, err := service.ListAuto(workerCtx, worker)
	if err != nil || len(listed) != 1 || listed[0].Task.ID != "auto-task" || listed[0].HarnessSelection != "enforced" || listed[0].Confinement != "none" || listed[0].Auth != "byoa" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	claimed, err := service.ClaimAuto(workerCtx, worker, listed[0].Order.ID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "child-token"})
	if err != nil || claimed.WorkerID != worker.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	deadline := claimed.ExecutionDeadline
	now = now.Add(10 * time.Second)
	renewed, err := service.Renew(workerCtx, worker, claimed.ID)
	if err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	released, err := service.Release(workerCtx, worker, claimed.ID, "test exit")
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("released=%+v err=%v", released, err)
	}

	createOrder("submitted-task", core.TaskModeAuto)
	submittedClaim, err := service.ClaimAuto(workerCtx, worker, "submitted-task-implement-1", core.WorkOrderClaim{SessionID: "session-b", ClientToken: "submitted-token"})
	if err != nil {
		t.Fatal(err)
	}
	submittedClaim.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(workerCtx, submittedClaim); err != nil {
		t.Fatal(err)
	}
	if submitted, renewErr := service.Renew(workerCtx, worker, submittedClaim.ID); renewErr != nil || submitted.State != core.WorkOrderSubmitted || !submitted.ExecutionDeadline.Equal(submittedClaim.ExecutionDeadline) {
		t.Fatalf("submitted renew=%+v err=%v", submitted, renewErr)
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
	task := core.Task{ID: "route-health", Workspace: "demo", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "route-health-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if listed, err := service.ListAuto(ctx, worker); err != nil || len(listed) != 0 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if _, err := service.ClaimAuto(ctx, worker, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token"}); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("claim error=%v", err)
	}
}

func workerTestConfig() *config.Config {
	harness := config.Harness{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}, "review": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}}}}
}

func TestAdversarialReviewPanelPinsWorkerSeatsAndAggregatesOneBounce(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.MaxBounces = 2
	cfg.Harnesses = append(cfg.Harnesses, config.Harness{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second})
	cfg.Review = config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}, {Model: "claude-review", Harness: "claude"}}}
	cfg.Repos = []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main"}}
	task := core.Task{ID: "panel-task", Workspace: "demo", Repo: "app", Mode: core.TaskModeAuto, PolicyVersion: 1, MergeApproval: true, State: core.TaskQueued, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.UseDurableQueue()
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatalf("review dispatch redelivery: %v", err)
	}
	if persisted, _ := st.ListTaskWorkOrders(ctx, task.ID); len(persisted) != 2 {
		t.Fatalf("dispatch redelivery created duplicate seats: %+v", persisted)
	}
	workOrders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	worker := core.Worker{ID: "worker-panel", Workspace: "demo", Name: "panel worker", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "claude", Healthy: true}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	unhealthy := worker
	unhealthy.Probes = []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "claude", Healthy: false}}
	if blocked, listErr := service.ListAuto(ctx, unhealthy); listErr != nil || len(blocked) != 0 {
		t.Fatalf("unhealthy panel harness was dispatchable: %+v err=%v", blocked, listErr)
	}
	listed, err := service.ListAuto(ctx, worker)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	bySeat := map[int]DispatchOrder{}
	for _, item := range listed {
		bySeat[item.Order.ReviewSeat] = item
	}
	if bySeat[1].Model != "gpt-review" || bySeat[1].Harness.Name != "codex" || bySeat[2].Model != "claude-review" || bySeat[2].Harness.Name != "claude" {
		t.Fatalf("seat dispatch=%+v", bySeat)
	}
	first, err := service.ClaimAuto(ctx, worker, bySeat[1].Order.ID, core.WorkOrderClaim{SessionID: "review-session-1", ClientToken: "review-token-1"})
	if err != nil || first.ModelEnforcement != "worker-pinned" || first.Model != "gpt-review" {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	if _, err = service.ClaimAuto(ctx, worker, bySeat[2].Order.ID, core.WorkOrderClaim{SessionID: "review-session-1", ClientToken: "review-token-2"}); err == nil || !strings.Contains(err.Error(), "session independence") {
		t.Fatalf("same-session claim error=%v", err)
	}
	second, err := service.ClaimAuto(ctx, worker, bySeat[2].Order.ID, core.WorkOrderClaim{SessionID: "review-session-2", ClientToken: "review-token-2"})
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
