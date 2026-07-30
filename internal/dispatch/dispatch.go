// Package dispatch advances the durable pipeline. Triage/spec execute inside
// conveyord; implementation and MCP-first review pause at leased work orders
// claimed by operator-owned agents (spec §21.4).
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type Dispatcher struct {
	Store           store.Store
	Cfg             *config.Config
	Pack            *pack.Bundle
	Agent           inprocess.Agent
	ConfigProvider  func(context.Context) (*config.Config, error)
	PublishIssue    func(context.Context, github.IssuePublication) (github.IssuePublicationResult, error)
	PublishReview   func(context.Context, github.ReviewPublication) (github.ReviewPublicationResult, error)
	ViewPullRequest func(context.Context, string, string) (github.PullRequest, error)
	RequestMerge    func(context.Context, string, int) error
	// ReviewDiff resolves the pushed task branch's diff against its base for
	// the in-process review fallback, which has no checkout of its own
	// (spec §21.4). Injectable for tests.
	ReviewDiff   func(context.Context, *config.Config, core.Task) (string, error)
	memoryQueue  chan queuedTask
	durableQueue bool
}

func New(st store.Store, cfg *config.Config, agent inprocess.Agent) *Dispatcher {
	return &Dispatcher{
		Store: st, Cfg: cfg, Agent: agent, memoryQueue: make(chan queuedTask, 64), durableQueue: st.IsDurable(),
		PublishIssue:    github.PublishIssue,
		PublishReview:   github.PublishReview,
		ViewPullRequest: github.PullRequestForBranch,
		RequestMerge:    github.MergePullRequest,
		ReviewDiff:      reviewBranchDiff,
	}
}

// reviewBranchDiff reads the branch diff from the shared bare cache; the
// implementing agent has already pushed the task branch to origin by the time
// review dispatches (spec §21.8).
func reviewBranchDiff(ctx context.Context, cfg *config.Config, task core.Task) (string, error) {
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return "", fmt.Errorf("repository %q is not configured", task.Repo)
	}
	return gitx.NewManager(cfg.CacheDir, "").BranchDiff(ctx, repo.URL, task.Branch, task.BaseBranch)
}

type queuedTask struct{ Workspace, TaskID string }

type MergeReadiness struct {
	State   string `json:"state"`
	HeadSHA string `json:"head_sha,omitempty"`
	URL     string `json:"url,omitempty"`
	Number  int    `json:"number,omitempty"`
}

const (
	maxModelAttachmentBytes = 25 << 20
	maxModelImageBytes      = 20 << 20
	maxModelFileBytes       = 50 << 20
	// maxModelDiffBytes caps the branch diff embedded inline in the
	// in-process review prompt at the same per-input ceiling as a single
	// model attachment (spec §21.4).
	maxModelDiffBytes = maxModelAttachmentBytes
)

func (d *Dispatcher) Enqueue(ctx context.Context, taskID string) {
	if !d.durableQueue {
		workspace, _ := store.WorkspaceFromContext(ctx)
		d.memoryQueue <- queuedTask{Workspace: workspace, TaskID: taskID}
	}
}

// DisableMemoryQueueForTest prevents the memory-store dispatcher goroutine
// from consuming queued work in tests that exercise only the control surface.
// Production queue selection is derived exclusively from Store.IsDurable.
func (d *Dispatcher) DisableMemoryQueueForTest() { d.durableQueue = true }

// DispatchNow advances one task synchronously. The MCP submit_for_review tool
// uses this when review is configured in-process so its result includes the
// completed review instead of a polling instruction (spec §21.4).
func (d *Dispatcher) DispatchNow(ctx context.Context, taskID string) error {
	return d.runTask(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), taskID)
}

func (d *Dispatcher) Run(ctx context.Context) {
	if d.durableQueue {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-d.memoryQueue:
			workCtx := store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), item.Workspace)
			if err := d.runTask(workCtx, item.TaskID); err != nil {
				log.Printf("[task %s] dispatch failed: %v", item.TaskID, err)
			}
		}
	}
}

func (d *Dispatcher) currentConfig(ctx context.Context) (*config.Config, error) {
	if d.ConfigProvider != nil {
		return d.ConfigProvider(ctx)
	}
	if d.Cfg == nil {
		return nil, fmt.Errorf("dispatcher requires workspace config")
	}
	return d.Cfg, nil
}

func (d *Dispatcher) runTask(ctx context.Context, taskID string) error {
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.NextStage != core.StageImplement && task.NextStage != core.StageSpec {
		return d.runTaskForSnapshot(ctx, task)
	}
	// River is the sole durable production mailbox, so a task row has one
	// delivery owner. Lifecycle state writes inside the stage path serialize
	// through taskops; no session lock is held across ordinary database work.
	return d.runTaskForSnapshot(ctx, task)
}

func (d *Dispatcher) runTaskForSnapshot(ctx context.Context, task core.Task) error {
	if isBlueprintAnchor(task) {
		// A blueprint parent is a batch anchor, never implementation work
		// (spec §4.1). Its children own the implement orders.
		return nil
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	if task.NextStage == "" {
		return nil
	}
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	route, ok := cfg.Routing.Stages[string(task.NextStage)]
	if !ok {
		return fmt.Errorf("no route for stage %s", task.NextStage)
	}
	if task.NextStage == core.StageReview && route.Execution == config.ExecutionMCP {
		return d.createReviewRound(ctx, cfg, task, route)
	}
	// Newly dispatched specs are always MCP work orders, even when a stale
	// pre-§21.33 route snapshot still says in_process. The remaining StageSpec
	// handling in runInProcess is only for completion of calls that were already
	// in flight when the execution contract changed (spec §21.33).
	if task.NextStage == core.StageImplement || task.NextStage == core.StageSpec {
		if _, active, activeErr := d.activeWorkOrder(ctx, task.ID, task.NextStage, ""); activeErr != nil {
			return activeErr
		} else if active {
			return nil
		}
		return d.createWorkOrder(ctx, cfg, task, route, "")
	}
	return d.runInProcess(ctx, cfg, task, route)
}

func isBlueprintAnchor(task core.Task) bool {
	return task.ParentTaskID == "" &&
		task.State == core.TaskQueued &&
		task.NextStage == core.StageImplement &&
		len(task.Children) > 0
}

func (d *Dispatcher) activeImplementationWorkOrder(ctx context.Context, taskID, reasonCode string) (core.WorkOrder, bool, error) {
	return d.activeWorkOrder(ctx, taskID, core.StageImplement, reasonCode)
}

func (d *Dispatcher) activeWorkOrder(ctx context.Context, taskID string, stage core.Stage, reasonCode string) (core.WorkOrder, bool, error) {
	orders, err := d.Store.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		if order.Stage != stage || (reasonCode != "" && order.ReasonCode != reasonCode) {
			continue
		}
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed {
			return order, true, nil
		}
	}
	return core.WorkOrder{}, false, nil
}

func (d *Dispatcher) createReviewRound(ctx context.Context, cfg *config.Config, task core.Task, route config.StageRoute) error {
	prior, err := d.Store.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		return err
	}
	round := 1
	latestRound := 0
	for _, order := range prior {
		if order.Stage == core.StageReview && order.ReviewRound >= round {
			round = order.ReviewRound + 1
		}
		if order.Stage == core.StageReview && order.ReviewRound > latestRound {
			latestRound = order.ReviewRound
		}
	}
	// Durable queue redelivery must reuse any active snapshotted round. The task
	// remains queued until the first seat claim issues order.claim (spec §21.37).
	for _, order := range prior {
		if order.Stage == core.StageReview && order.ReviewRound == latestRound &&
			(order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted) {
			return nil
		}
	}
	jobs, orders, err := BuildReviewRound(cfg, task, route, round)
	if err != nil {
		return err
	}
	if _, err = taskops.ExecuteWorkOrder(ctx, d.Store, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, d.Store.CreateReviewRoundCommand(ctx, lease, task.ID, jobs, orders)
	}); err != nil {
		return err
	}
	if task.ApprovalStale {
		if err = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.refresh_round_created", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "approval-stale", "review_round": round, "review_scope": task.RefreshReviewScope, "approved_head": task.RefreshBaselineSHA, "new_head": task.RefreshHeadSHA})}); err != nil {
			return err
		}
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": core.StageReview, "execution": "mcp", "timeout": route.TimeoutText, "review_round": round, "seat_count": len(orders)})})
}

// BuildReviewRound snapshots the current workspace panel and harness routing
// into a new immutable round. Retry recovery uses this same constructor so it
// cannot accidentally reuse an expired seat's stale execution snapshot.
func BuildReviewRound(cfg *config.Config, task core.Task, route config.StageRoute, round int) ([]core.Job, []core.WorkOrder, error) {
	if cfg == nil || round <= 0 {
		return nil, nil, fmt.Errorf("review round configuration and positive round are required")
	}
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
		route = cfg.Routing.Stages["review"]
	}
	now := time.Now().UTC()
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	seats := cfg.Review.Seats
	if len(seats) == 0 {
		seats = []config.ReviewSeat{{Model: route.Model, Harness: route.Harness}}
	}
	jobs := make([]core.Job, 0, len(seats))
	orders := make([]core.WorkOrder, 0, len(seats))
	for i, seat := range seats {
		seatNumber := i + 1
		jobID := fmt.Sprintf("%s-review-%d-seat-%d", task.ID, round, seatNumber)
		harness := seat.Harness
		if harness == "" {
			harness = route.Harness
		}
		var harnessConfig *core.HarnessSnapshot
		if harness != "" {
			var ok bool
			harnessConfig, ok = reviewHarnessSnapshot(cfg, harness)
			if !ok {
				return nil, nil, fmt.Errorf("review seat %d references unavailable harness %q", seatNumber, harness)
			}
		}
		if harnessConfig != nil {
			harnessConfig.Effort = seat.Effort
		}
		jobs = append(jobs, core.Job{ID: jobID, TaskID: task.ID, Stage: core.StageReview, Harness: "external-mcp", ModelTier: seat.Model, AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending})
		orders = append(orders, core.WorkOrder{
			ID: jobID, TaskID: task.ID, JobID: jobID, Stage: core.StageReview,
			State: core.WorkOrderQueued, Claimable: true, SelfReported: true,
			ReviewRound: round, ReviewSeat: seatNumber, RequiredModel: seat.Model,
			ReviewKind: func() string {
				if task.ApprovalStale {
					return "refresh"
				}
				return ""
			}(),
			ReviewScope: task.RefreshReviewScope, BaselineSHA: task.RefreshBaselineSHA, HeadSHA: task.RefreshHeadSHA,
			RequiredHarness: harness, RequiredEffort: seat.Effort, RequiredHarnessConfig: harnessConfig,
			ExecutionTimeoutText: route.TimeoutText,
			QueueEnteredAt:       now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now,
		})
	}
	return jobs, orders, nil
}

func reviewHarnessSnapshot(cfg *config.Config, name string) (*core.HarnessSnapshot, bool) {
	for _, harness := range cfg.Harnesses {
		if harness.Name != name {
			continue
		}
		return &core.HarnessSnapshot{
			Name:                  harness.Name,
			MCPTransport:          harness.MCPTransport,
			MCPAttachment:         harness.MCPAttachment,
			Command:               append([]string(nil), harness.Command...),
			ModelArgs:             append([]string(nil), harness.ModelArgs...),
			DefaultModelSentinels: append([]string(nil), harness.DefaultModelSentinels...),
			EffortArgs:            cloneEffortArgs(harness.EffortArgs),
			ProbeCommand:          append([]string(nil), harness.ProbeCommand...),
			ProbeTimeoutText:      harness.ProbeTimeoutText,
			StallTimeoutText:      harness.StallTimeoutText,
		}, true
	}
	return nil, false
}

// BuildFutureWorkOrderRouting resolves one queued non-review order from the
// task's frozen setup contract. Setup reassignment uses the same constructor
// inputs as ordinary dispatch without creating a second routing shape
// (spec §21.35 changes 3-5).
func BuildFutureWorkOrderRouting(cfg *config.Config, task core.Task, stage core.Stage) (core.WorkOrder, error) {
	if cfg == nil || (stage != core.StageSpec && stage != core.StageImplement) {
		return core.WorkOrder{}, fmt.Errorf("future work routing requires spec or implementation stage")
	}
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	route, ok := cfg.Routing.Stages[string(stage)]
	if !ok || route.Execution != config.ExecutionMCP {
		return core.WorkOrder{}, fmt.Errorf("%s does not use an MCP execution route", stage)
	}
	var snapshot *core.HarnessSnapshot
	if route.Harness != "" {
		var found bool
		snapshot, found = reviewHarnessSnapshot(cfg, route.Harness)
		if !found {
			return core.WorkOrder{}, fmt.Errorf("%s route references unavailable harness %q", stage, route.Harness)
		}
		if route.Effort != "" {
			snapshot.Effort = route.Effort
			snapshot.EffortArgv = append([]string(nil), snapshot.EffortArgs[route.Effort]...)
		}
	}
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	now := time.Now().UTC()
	return core.WorkOrder{Stage: stage, RequiredModel: cfg.EffectiveModel(string(stage)), RequiredHarness: route.Harness,
		RequiredEffort: route.Effort, RequiredHarnessConfig: snapshot, ExecutionTimeoutText: route.TimeoutText,
		QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout)}, nil
}

func cloneEffortArgs(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for effort, args := range source {
		result[effort] = append([]string(nil), args...)
	}
	return result
}

func (d *Dispatcher) createWorkOrder(ctx context.Context, cfg *config.Config, task core.Task, route config.StageRoute, reasonCode string) error {
	prior, err := d.Store.ListJobs(ctx, task.ID)
	if err != nil {
		return err
	}
	attempt := 1
	for _, job := range prior {
		if job.Stage == task.NextStage {
			attempt++
		}
	}
	jobID := fmt.Sprintf("%s-%s-%d", task.ID, task.NextStage, attempt)
	effectiveModel := cfg.EffectiveModel(string(task.NextStage))
	job := core.Job{ID: jobID, TaskID: task.ID, Stage: task.NextStage, Harness: "external-mcp", ModelTier: effectiveModel, AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	now := time.Now().UTC()
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	var harnessConfig *core.HarnessSnapshot
	if route.Harness != "" {
		var found bool
		harnessConfig, found = reviewHarnessSnapshot(cfg, route.Harness)
		if !found {
			return fmt.Errorf("%s route references unavailable harness %q", task.NextStage, route.Harness)
		}
		if route.Effort != "" {
			harnessConfig.Effort = route.Effort
			harnessConfig.EffortArgv = append([]string(nil), harnessConfig.EffortArgs[route.Effort]...)
		}
	}
	order := core.WorkOrder{
		ID: jobID, TaskID: task.ID, JobID: jobID, Stage: task.NextStage,
		State: core.WorkOrderQueued, Claimable: true, SelfReported: true,
		RequiredModel: effectiveModel, RequiredHarness: route.Harness,
		RequiredEffort: route.Effort, RequiredHarnessConfig: harnessConfig,
		ReasonCode: reasonCode, BaselineSHA: task.ApprovedHeadSHA,
		ExecutionTimeoutText: route.TimeoutText,
		QueueEnteredAt:       now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now,
	}
	created, err := taskops.ExecuteWorkOrder(ctx, d.Store, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) {
		return d.Store.CreateStageWorkOrderCommand(ctx, lease, job, order)
	})
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	payload := map[string]any{
		"stage": task.NextStage, "execution": "mcp", "timeout": route.TimeoutText,
		"harness": route.Harness, "model": effectiveModel, "model_policy": route.ModelPolicy,
	}
	if reasonCode != "" {
		payload["reason_code"] = reasonCode
	}
	if route.Effort != "" {
		payload["required_effort"] = route.Effort
		payload["effort_argv"] = append([]string(nil), harnessConfig.EffortArgv...)
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: jobID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(payload)})
}

func (d *Dispatcher) runInProcess(ctx context.Context, cfg *config.Config, task core.Task, route config.StageRoute) error {
	if d.Agent == nil {
		return fmt.Errorf("in-process agent is not configured")
	}
	prior, err := d.Store.ListJobs(ctx, task.ID)
	if err != nil {
		return err
	}
	attempt := 1
	for _, job := range prior {
		if job.Stage == task.NextStage {
			attempt++
		}
	}
	jobID := fmt.Sprintf("%s-%s-%d", task.ID, task.NextStage, attempt)
	if task.NextStage == core.StageReview && len(cfg.Review.Seats) == 1 {
		route.Model = cfg.Review.Seats[0].Model
	}
	now := time.Now().UTC()
	job := core.Job{ID: jobID, TaskID: task.ID, Stage: task.NextStage, Harness: "openai-responses", ModelTier: route.Model, AuthMode: "deployment-key", Runner: "in-process", Confinement: "control-plane", State: core.JobRunning, StartedAt: now}
	input, err := d.buildStageInput(ctx, cfg, task.NextStage, task)
	if err != nil {
		attachmentCount, attachmentTypes := d.modelInputArtifactSummary(ctx, task)
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "artifact.context_failed", Payload: core.JSONPayload(map[string]any{
			"stage": task.NextStage, "phase": "attachment_preparation", "provider": "openai_responses", "model": route.Model,
			"attachment_count": attachmentCount, "attachment_types": attachmentTypes, "error": err.Error(),
		})})
		return err
	}
	input.Effort = route.Effort
	if _, err := taskops.New(d.Store).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		return err
	}
	if err := d.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	stageCtx, cancel := context.WithTimeout(ctx, route.Timeout)
	defer cancel()
	result, runErr := d.Agent.Run(stageCtx, route.Model, input)
	job.EndedAt = time.Now().UTC()
	job.TokensIn = result.TokensIn
	job.TokensOut = result.TokensOut
	if len(result.Transcript) != 0 {
		sum := sha256.Sum256(result.Transcript)
		id := fmt.Sprintf("%x", sum)
		artifact, artifactErr := d.Store.CreateArtifact(ctx, core.Artifact{ID: id, Workspace: task.Workspace, Name: job.ID + "-transcript.json", ContentType: "application/json", SizeBytes: int64(len(result.Transcript)), Role: core.ArtifactRoleGeneratedAudit, TaskID: task.ID}, result.Transcript)
		if artifactErr == nil {
			_ = d.Store.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "artifact://" + artifact.ID, RedactionStats: result.Redactions, CreatedAt: time.Now().UTC()})
		}
	}
	if runErr != nil {
		job.State = core.JobFailed
		_ = d.Store.UpdateJob(ctx, job)
		failure := map[string]any{"error": runErr.Error()}
		if result.Diagnostic != nil {
			failure["diagnostic"] = result.Diagnostic
		}
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.failed", Payload: core.JSONPayload(failure)})
		return d.transition(ctx, task.ID, core.TaskJobFail, "", task.NextStage)
	}
	job.State = core.JobDone
	if err := d.Store.UpdateJob(ctx, job); err != nil {
		return err
	}
	return d.completeOutput(ctx, cfg, task, job, result.Output, "in-process")
}

func (d *Dispatcher) modelInputArtifactSummary(ctx context.Context, task core.Task) (int, []string) {
	artifacts, err := d.Store.ListArtifacts(ctx)
	if err != nil {
		return 0, nil
	}
	types := []string{}
	for _, artifact := range artifacts {
		if artifact.TaskID != task.ID && (task.FeatureID == "" || artifact.FeatureID != task.FeatureID) {
			continue
		}
		if !artifact.Role.ModelInputEligible() {
			continue
		}
		types = append(types, strings.ToLower(strings.TrimSpace(artifact.ContentType)))
	}
	sort.Strings(types)
	return len(types), types
}

func (d *Dispatcher) buildStageInput(ctx context.Context, cfg *config.Config, stage core.Stage, task core.Task) (inprocess.Input, error) {
	role, err := d.Pack.Role(stage)
	if err != nil {
		return inprocess.Input{}, err
	}
	if stage == core.StageReview {
		role = pack.InProcessReviewRole(role)
	}
	input := inprocess.Input{}
	var prompt strings.Builder
	prompt.WriteString(role)
	fmt.Fprintf(&prompt, "\n\n# Task %s: %s\n\nSpec approval: %t · Merge approval: %t · Repository: %s\n\n%s\n\nBranch: %s (base %s).\n", task.ID, task.Title, task.SpecApproval, task.MergeApproval, task.Repo, task.Body, task.Branch, task.BaseBranch)
	if stage == core.StageTriage {
		requirements, _ := d.Store.ListRequirements(ctx)
		prompt.WriteString("\n# Requirement corpus\n\nPropose only an ID from this list, or an empty requirement_id:\n")
		for _, requirement := range requirements {
			fmt.Fprintf(&prompt, "- %s: %s\n", requirement.ID, requirement.Title)
		}
	}
	events, _ := d.Store.ListEvents(ctx, task.ID)
	invalidKind := string(stage) + ".output_invalid"
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != invalidKind {
			continue
		}
		var prior struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(events[i].Payload, &prior) == nil && prior.Error != "" {
			fmt.Fprintf(&prompt, "\n# Previous output rejected\n\nCorrect this validation error in the next response: %s\n", prior.Error)
		}
		break
	}
	if stage == core.StageImplement || stage == core.StageReview {
		spec, exists, getErr := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if getErr != nil {
			return inprocess.Input{}, getErr
		}
		if exists && spec.Approved {
			fmt.Fprintf(&prompt, "\n# Approved specification v%d\n\n%s\n", spec.Version, spec.Content)
		}
	}
	if stage == core.StageReview {
		// The in-process reviewer has no checkout, so the change under review
		// must travel in the prompt itself; a missing or oversized diff fails
		// before model execution instead of degrading to a diff-less review
		// (spec §21.4).
		if d.ReviewDiff == nil {
			return inprocess.Input{}, fmt.Errorf("in-process review for task %s requires a branch diff resolver", task.ID)
		}
		diff, diffErr := d.ReviewDiff(ctx, cfg, task)
		if diffErr != nil {
			return inprocess.Input{}, fmt.Errorf("resolve branch diff for task %s: %w", task.ID, diffErr)
		}
		if len(diff) > maxModelDiffBytes {
			return inprocess.Input{}, fmt.Errorf("branch diff for task %s (%d bytes) exceeds the %d-byte model input limit", task.ID, len(diff), maxModelDiffBytes)
		}
		if strings.TrimSpace(diff) == "" {
			fmt.Fprintf(&prompt, "\n# Branch diff\n\nBranch %s contains no changes against base %s.\n", task.Branch, task.BaseBranch)
		} else {
			// Four-backtick fence so diff hunks that themselves contain
			// three-backtick lines cannot terminate the block early.
			fmt.Fprintf(&prompt, "\n# Branch diff (%s vs %s)\n\nThe change under review:\n\n````diff\n%s\n````\n", task.Branch, task.BaseBranch, strings.TrimRight(diff, "\n"))
		}
	}
	if stage == core.StageSpec {
		input.OutputSchema = &inprocess.OutputSchema{Name: "conveyor_spec", Schema: pipeline.StructuredSpecSchema()}
		// A spec-gate redirect reopens this stage, so the regeneration must see
		// the declined revision and the reviewer's comments — the same feedback
		// the MCP work-order context already threads to implementing agents.
		spec, exists, getErr := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if getErr != nil {
			return inprocess.Input{}, getErr
		}
		if exists {
			fmt.Fprintf(&prompt, "\n# Prior specification revision v%d (declined at the human gate)\n\nProduce the next revision of this document; do not start over.\n\n%s\n", spec.Version, spec.Content)
		}
		interventions, listErr := d.Store.ListInterventions(ctx, task.ID)
		if listErr != nil {
			return inprocess.Input{}, listErr
		}
		wroteHeader := false
		for _, item := range interventions {
			if item.Action != core.InterventionRedirect || strings.TrimSpace(item.Comment) == "" {
				continue
			}
			if !wroteHeader {
				prompt.WriteString("\n# Human gate feedback\n\nApply every correction below to the next revision. Where feedback conflicts with the task body or the prior revision, the feedback wins.\n")
				wroteHeader = true
			}
			fmt.Fprintf(&prompt, "\n---\n\n%s\n", item.Comment)
		}
	}
	artifacts, err := d.Store.ListArtifacts(ctx)
	if err != nil {
		return inprocess.Input{}, fmt.Errorf("list context artifacts for task %s: %w", task.ID, err)
	}
	seen := map[string]bool{}
	totalBytes := 0
	for _, artifact := range artifacts {
		if artifact.TaskID != task.ID && (task.FeatureID == "" || artifact.FeatureID != task.FeatureID) {
			continue
		}
		if !artifact.Role.ModelInputEligible() {
			continue
		}
		if seen[artifact.ID] {
			continue
		}
		seen[artifact.ID] = true
		resolved, content, getErr := d.Store.GetArtifactForContext(ctx, artifact.ID, task.ID, task.FeatureID)
		if getErr != nil {
			return inprocess.Input{}, fmt.Errorf("read context artifact %s for task %s: %w", artifact.ID, task.ID, getErr)
		}
		if len(content) > maxModelAttachmentBytes {
			return inprocess.Input{}, fmt.Errorf("context artifact %s (%s) exceeds the %d-byte model attachment limit", resolved.ID, resolved.Name, maxModelAttachmentBytes)
		}
		kind, kindErr := modelAttachmentKind(resolved)
		if kindErr != nil {
			return inprocess.Input{}, kindErr
		}
		if kind == inprocess.AttachmentImage && len(content) > maxModelImageBytes {
			return inprocess.Input{}, fmt.Errorf("image artifact %s (%s) exceeds the %d-byte image input limit", resolved.ID, resolved.Name, maxModelImageBytes)
		}
		totalBytes += len(content)
		if totalBytes > maxModelFileBytes {
			return inprocess.Input{}, fmt.Errorf("context artifacts for task %s exceed the %d-byte combined model input limit", task.ID, maxModelFileBytes)
		}
		fmt.Fprintf(&prompt, "\nContext artifact supplied as %s input: %s (%s, %d bytes, id %s)\n", kind, resolved.Name, resolved.ContentType, len(content), resolved.ID)
		input.Attachments = append(input.Attachments, inprocess.Attachment{ID: resolved.ID, Name: resolved.Name, ContentType: resolved.ContentType, Kind: kind, Content: content})
	}
	input.Prompt = prompt.String()
	return input, nil
}

// modelAttachmentKind is the provider boundary for in-process pipeline context:
// text/documents use Responses input_file, images use input_image, and audio is
// transcribed before the stage request. Anything outside that documented set
// fails before model execution instead of degrading to metadata (spec §21.4).
func modelAttachmentKind(artifact core.Artifact) (inprocess.AttachmentKind, error) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(artifact.ContentType, ";")[0]))
	ext := strings.ToLower(filepath.Ext(artifact.Name))
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return inprocess.AttachmentImage, nil
	}
	if strings.HasPrefix(contentType, "audio/") || contentType == "application/ogg" {
		switch ext {
		case ".flac", ".mp3", ".mp4", ".mpeg", ".mpga", ".m4a", ".ogg", ".wav", ".webm":
			return inprocess.AttachmentAudio, nil
		}
		return "", fmt.Errorf("unsupported audio attachment %s (%s): extension %s is not transcribable", artifact.ID, artifact.Name, ext)
	}
	if strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml" || contentType == "application/pdf" {
		return inprocess.AttachmentDocument, nil
	}
	switch ext {
	case ".txt", ".md", ".json", ".html", ".xml", ".csv", ".tsv", ".pdf", ".doc", ".docx", ".rtf", ".odt", ".ppt", ".pptx", ".xls", ".xlsx":
		return inprocess.AttachmentDocument, nil
	}
	return "", fmt.Errorf("unsupported context artifact %s (%s, %s); pipeline context was not prepared", artifact.ID, artifact.Name, artifact.ContentType)
}

func (d *Dispatcher) completeOutput(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, output, reviewer string) error {
	invalid := func(parseErr error) error {
		kind := string(job.Stage) + ".output_invalid"
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: kind, Payload: core.JSONPayload(map[string]string{"error": parseErr.Error()})}); err != nil {
			return err
		}
		count, _ := d.Store.CountEventsSinceHumanIntervention(ctx, task.ID, kind)
		if count >= cfg.MaxBounces {
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": kind, "window": count, "max_bounces": cfg.MaxBounces})})
			return d.transition(ctx, task.ID, core.TaskStageBounceLimit, "", job.Stage)
		}
		return d.transition(ctx, task.ID, core.TaskStageBounce, job.Stage, "")
	}
	switch job.Stage {
	case core.StageTriage:
		result, err := pipeline.ParseTriage(output)
		if err != nil {
			return invalid(err)
		}
		if err = d.Store.UpdateTaskClassification(ctx, task.ID, result.Class); err != nil {
			return err
		}
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "triage.completed", Payload: core.JSONPayload(result)})
		d.recordRequirementSuggestion(ctx, task, result.RequirementID)
		if task.Level == core.L3 || result.Route == "human" {
			return d.transition(ctx, task.ID, core.TaskTriageRouteHuman, "", core.StageTriage)
		}
		if result.Route == "parked" {
			return d.transition(ctx, task.ID, core.TaskTriagePark, "", core.StageTriage)
		}
		next := core.StageImplement
		if (task.PolicyVersion > 0 && task.SpecApproval) || (task.PolicyVersion == 0 && task.Level == core.L2) || result.Route == "spec" {
			next = core.StageSpec
		}
		return d.transition(ctx, task.ID, core.TaskStageAdvance, next, "")
	case core.StageSpec:
		result, err := pipeline.RenderStructuredSpec(output)
		if err != nil {
			return invalid(err)
		}
		return d.completeSpec(ctx, task, result, reviewer, job.ModelTier)
	case core.StageReview:
		result, err := pipeline.ParseReview(output)
		if err != nil {
			return invalid(err)
		}
		return d.applyReview(ctx, cfg, task, job, result, reviewer, job.ID, "", job.ModelTier)
	default:
		return fmt.Errorf("unsupported in-process stage %s", job.Stage)
	}
}

func (d *Dispatcher) ApplyExternalReview(ctx context.Context, task core.Task, job core.Job, result pipeline.Review, reviewWorkOrderID, session, model string) error {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	return d.applyReview(ctx, cfg, task, job, result, "external-mcp", reviewWorkOrderID, session, model)
}

// ApplyExternalSpec validates an MCP-authored structured specification and
// enters the unchanged approval/auto-approval path (spec §21.33).
func (d *Dispatcher) ApplyExternalSpec(ctx context.Context, task core.Task, job core.Job, value pipeline.StructuredSpec, agent, model string) (core.SpecVersion, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return core.SpecVersion{}, err
	}
	result, err := pipeline.RenderStructuredSpec(string(raw))
	if err != nil {
		return core.SpecVersion{}, err
	}
	return d.completeSpecVersion(ctx, task, result, agent, model)
}

// CreatePlanningBlueprint materializes a planning-agent draft onto the same
// task/spec contract used by every other blueprint. The planning session is an
// intake surface, not an approval surface: completeSpecVersion leaves the new
// version at the unchanged spec gate when workspace policy requires it
// (spec §§9, 13.1, 21.46).
func (d *Dispatcher) CreatePlanningBlueprint(
	ctx context.Context,
	sessionID, taskID, title, repoName string,
	value pipeline.StructuredSpec,
	model string,
) (core.Task, core.SpecVersion, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	result, err := pipeline.RenderStructuredSpec(string(raw))
	if err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	repo, ok := cfg.Repo(strings.TrimSpace(repoName))
	if !ok {
		return core.Task{}, core.SpecVersion{}, fmt.Errorf("repository %q is not configured", repoName)
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 {
		return core.Task{}, core.SpecVersion{}, fmt.Errorf("blueprint title must be between 1 and 200 characters")
	}
	if existing, getErr := d.Store.GetTask(ctx, taskID); getErr == nil {
		if existing.Source != "planning:"+sessionID || existing.Title != title || existing.Repo != repo.Name {
			return core.Task{}, core.SpecVersion{}, fmt.Errorf("planning blueprint task %s already exists with different input", taskID)
		}
		version, exists, specErr := d.Store.GetLatestSpecVersion(ctx, taskID)
		if specErr != nil {
			return core.Task{}, core.SpecVersion{}, specErr
		}
		if !exists || version.Content != result.Markdown {
			return core.Task{}, core.SpecVersion{}, fmt.Errorf("planning blueprint task %s already exists without the identical spec", taskID)
		}
		return existing, version, nil
	}
	setup, ok := cfg.Setup("")
	if !ok {
		return core.Task{}, core.SpecVersion{}, fmt.Errorf("workspace default setup is unavailable")
	}
	effective := cfg.WithSetup(setup)
	workspace, _ := store.WorkspaceFromContext(ctx)
	task := core.Task{
		ID: taskID, Workspace: workspace, Source: "planning:" + sessionID,
		Title: title, Body: result.Markdown, Class: "feature",
		SpecApproval: effective.Execution.SpecApproval, MergeApproval: effective.Execution.MergeApproval,
		PolicyVersion: 1, SetupName: setup.Name, SetupContract: setup,
		Repo: repo.Name, BaseBranch: repo.Base, Branch: gitx.BranchName(taskID),
		State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Now().UTC(),
	}
	if err = d.Store.CreateTaskWithDependencies(ctx, task, nil); err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	version, err := d.completeSpecVersion(ctx, task, result, "planning-agent", model)
	if err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return core.Task{}, core.SpecVersion{}, err
	}
	if session, sessionErr := d.Store.GetPlanningSession(ctx, sessionID); sessionErr == nil {
		// Planning in a requirement's context proposes the same advisory,
		// human-confirmed relation as triage. The event is the durable source;
		// the generalized link is a projection (spec §4.2 item 1, §9).
		d.recordRequirementSuggestion(ctx, current, session.RequirementContextID)
	}
	return current, version, nil
}

func (d *Dispatcher) completeSpec(ctx context.Context, task core.Task, result pipeline.Spec, agent, model string) error {
	_, err := d.completeSpecVersion(ctx, task, result, agent, model)
	return err
}

func (d *Dispatcher) completeSpecVersion(ctx context.Context, task core.Task, result pipeline.Spec, agent, model string) (core.SpecVersion, error) {
	version, err := d.Store.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: result.Markdown, AcceptanceCount: len(result.Acceptance), Acceptance: core.JSONPayload(result.Acceptance), Decomposition: core.JSONPayload(result.Decomposition), Agent: agent, Model: model})
	if err != nil {
		return core.SpecVersion{}, err
	}
	if (task.PolicyVersion > 0 && task.SpecApproval) || (task.PolicyVersion == 0 && task.Level == core.L2) {
		return version, d.transition(ctx, task.ID, core.TaskGateSpec, "", core.StageImplement)
	}
	children, err := d.Store.ApproveSpecVersionAndMaterialize(ctx, task.ID, version.Version)
	if err != nil {
		return core.SpecVersion{}, err
	}
	if err = d.queueApprovedIssue(ctx, task, version); err != nil {
		return core.SpecVersion{}, err
	}
	if err = d.transition(ctx, task.ID, core.TaskStageAdvance, core.StageImplement, ""); err != nil {
		return core.SpecVersion{}, err
	}
	for _, child := range children {
		d.Enqueue(ctx, child.ID)
	}
	return version, nil
}

func (d *Dispatcher) applyReview(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, result pipeline.Review, reviewer, reviewWorkOrderID, session, model string) error {
	if reviewWorkOrderID == "" {
		reviewWorkOrderID = job.ID
	}
	if model == "" {
		model = job.ModelTier
	}
	reviewedCommitSHA := ""
	events, _ := d.Store.ListEvents(ctx, task.ID)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var pullRequest struct {
			HeadSHA string `json:"head_sha"`
		}
		if json.Unmarshal(events[i].Payload, &pullRequest) == nil && pullRequest.HeadSHA != "" {
			reviewedCommitSHA = pullRequest.HeadSHA
			break
		}
	}
	implementModel := ""
	jobs, _ := d.Store.ListJobs(ctx, task.ID)
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Stage == core.StageImplement {
			implementModel = jobs[i].ModelTier
			break
		}
	}
	same := "unknown"
	if model != "" && implementModel != "" {
		same = fmt.Sprintf("%t", model == implementModel)
	}
	repo, publicationEligible := cfg.Repo(task.Repo)
	publicationEligible = publicationEligible && repo.GitHub != ""
	round, seat := 0, 0
	requiredModel, requiredHarness, requiredEffort, enforcement := model, "", "", "self-reported"
	decisionReviewKind, decisionReviewScope, decisionBaseline, decisionHead := "", "", "", ""
	if order, orderErr := d.Store.GetWorkOrder(ctx, reviewWorkOrderID); orderErr == nil {
		round, seat = order.ReviewRound, order.ReviewSeat
		requiredModel, requiredHarness, requiredEffort = order.RequiredModel, order.RequiredHarness, order.RequiredEffort
		if order.ModelEnforcement != "" {
			enforcement = order.ModelEnforcement
		}
		decisionReviewKind, decisionReviewScope = order.ReviewKind, order.ReviewScope
		decisionBaseline, decisionHead = order.BaselineSHA, order.HeadSHA
	}
	if err := taskops.New(d.Store).AcceptReviewDecision(ctx, core.ReviewDecision{
		TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: reviewWorkOrderID,
		Verdict: result.Verdict, ReasonCode: result.ReasonCode, Summary: result.Summary,
		Feedback: result.Feedback, ReviewedCommitSHA: reviewedCommitSHA, Reviewer: reviewer,
		ReviewerModel: model, ReviewerSession: "distinct", SameModelAsImplementer: same,
		ReviewRound: round, ReviewSeat: seat, RequiredModel: requiredModel,
		ReviewKind: decisionReviewKind, ReviewScope: decisionReviewScope, BaselineSHA: decisionBaseline, HeadSHA: decisionHead,
		RequiredHarness: requiredHarness, RequiredEffort: requiredEffort, ModelEnforcement: enforcement,
		InterventionActorID: "review:" + session, PublicationEligible: publicationEligible,
		Level: task.Level, PolicyVersion: task.PolicyVersion, MergeApproval: task.MergeApproval, MaxBounces: cfg.MaxBounces,
	}); err != nil {
		return err
	}
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.State == core.TaskQueued {
		d.Enqueue(ctx, task.ID)
	}
	if current.State == core.TaskApproved && current.PolicyVersion > 0 && !current.MergeApproval {
		if err := d.MergeApprovedTask(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) bounce(ctx context.Context, cfg *config.Config, taskID, jobID, reason, feedback string) error {
	count, _ := d.Store.CountEvents(ctx, taskID, "pipeline.bounced")
	count++
	// The check-in comparison uses bounces since the last human intervention,
	// not the lifetime count (spec §21.17).
	window, _ := d.Store.CountEventsSinceHumanIntervention(ctx, taskID, "pipeline.bounced")
	window++
	_ = d.Store.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement", "reason_code": reason, "feedback": feedback, "count": count, "source": "mcp-review"})})
	if window >= cfg.MaxBounces {
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"count": count, "window": window, "max_bounces": cfg.MaxBounces})})
		return d.transition(ctx, taskID, core.TaskStageBounceLimit, "", core.StageImplement)
	}
	return d.transition(ctx, taskID, core.TaskStageBounce, core.StageImplement, "")
}

// recordRequirementSuggestion proposes a requirement relation for a stray task.
// It replaces the retired triage.feature_suggested event (spec §4.2 item 1,
// §21.46 change 5). The event is the durable proposal — links are projections
// of events, and a requirement relation is machinery-suggested and
// human-confirmed, never volunteered as a standing edge by an agent.
func (d *Dispatcher) recordRequirementSuggestion(ctx context.Context, task core.Task, requirementID string) {
	requirementID = strings.TrimSpace(requirementID)
	if requirementID == "" {
		return
	}
	requirements, _ := d.Store.ListRequirements(ctx)
	for _, requirement := range requirements {
		if requirement.ID == requirementID {
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "task.requirement_suggested", Payload: core.JSONPayload(map[string]string{
				"requirement_id": requirement.ID, "requirement_slug": requirement.Slug, "requirement_title": requirement.Title,
			})})
			return
		}
	}
}

func (d *Dispatcher) transition(ctx context.Context, taskID string, command core.TaskCommand, next, recovery core.Stage) error {
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	destination, err := core.TransitionTask(task.State, command)
	if err != nil {
		return err
	}
	outcome, err := taskops.New(d.Store).Perform(ctx, taskID, taskops.Command{
		Kind: command, NextStage: next, RecoveryStage: recovery, ProjectStages: true,
	})
	if err != nil {
		return err
	}
	if destination == core.TaskQueued && !outcome.Enqueued {
		d.Enqueue(ctx, taskID)
	}
	return nil
}

func (d *Dispatcher) HandleIntervention(ctx context.Context, task core.Task, latest core.Job, intervention core.Intervention) error {
	switch intervention.Action {
	case core.InterventionCancel:
		_, err := taskops.New(d.Store).Cancel(ctx, intervention)
		return err
	case core.InterventionReject:
		return d.transition(ctx, task.ID, core.TaskInterventionReject, "", "")
	case core.InterventionApprove:
		if latest.Stage == core.StageSpec {
			spec, ok, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("task %s has no spec", task.ID)
			}
			children, materializeErr := d.Store.ApproveSpecVersionAndMaterialize(ctx, task.ID, spec.Version)
			if materializeErr != nil {
				return materializeErr
			}
			if err = d.queueApprovedIssue(ctx, task, spec); err != nil {
				return err
			}
			if err = d.transition(ctx, task.ID, core.TaskInterventionApproveSpec, core.StageImplement, ""); err != nil {
				return err
			}
			for _, child := range children {
				d.Enqueue(ctx, child.ID)
			}
			return nil
		}
		head := task.ReviewedHeadSHA
		if head == "" {
			head = d.reviewedHeadFromEvents(ctx, task.ID)
		}
		if err := d.Store.BindTaskApproval(ctx, task.ID, head); err != nil {
			return err
		}
		return d.transition(ctx, task.ID, core.TaskInterventionApproveReview, "", "")
	case core.InterventionRedirect:
		target := task.RecoveryStage
		if target == "" {
			target = latest.Stage
		}
		if latest.Stage == core.StageSpec {
			// RecoveryStage identifies where approval continues, so a spec gate
			// normally points at implementation. Requested changes instead reopen
			// the existing spec workflow and require a newly approved revision.
			target = core.StageSpec
		} else if latest.Stage == core.StageReview {
			target = core.StageImplement
		}
		return d.transition(ctx, task.ID, core.TaskInterventionRedirect, target, "")
	case core.InterventionPull:
		return nil
	}
	return nil
}

func (d *Dispatcher) reviewedHeadFromEvents(ctx context.Context, taskID string) string {
	events, _ := d.Store.ListEvents(ctx, taskID)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" && events[i].Kind != "review.completed" {
			continue
		}
		var payload struct {
			ApprovedHeadSHA   string `json:"approved_head_sha"`
			ReviewedCommitSHA string `json:"reviewed_commit_sha"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil {
			if payload.ApprovedHeadSHA != "" {
				return payload.ApprovedHeadSHA
			}
			if payload.ReviewedCommitSHA != "" {
				return payload.ReviewedCommitSHA
			}
		}
	}
	return ""
}

func (d *Dispatcher) refreshScope(task core.Task, conflict bool) string {
	scope := task.SetupContract.RefreshReview
	if scope == "" {
		scope = config.RefreshReviewDelta
	}
	if conflict && scope == config.RefreshReviewNone {
		scope = config.RefreshReviewDelta
	}
	return scope
}

func (d *Dispatcher) beginRefreshLocked(ctx context.Context, task core.Task, newHead, reason string, conflict bool) error {
	baseline := task.ApprovedHeadSHA
	if baseline == "" {
		baseline = task.ReviewedHeadSHA
	}
	if baseline == "" || newHead == "" || baseline == newHead {
		return fmt.Errorf("task %s requires distinct approved and current heads for refresh", task.ID)
	}
	scope := d.refreshScope(task, conflict)
	if err := d.Store.MarkTaskApprovalStale(ctx, task.ID, baseline, newHead, scope, reason); err != nil {
		return err
	}
	if scope == config.RefreshReviewNone && !conflict {
		return d.Store.SkipTaskRefresh(ctx, task.ID, newHead, "clean-update")
	}
	if err := d.transition(ctx, task.ID, core.TaskRefreshReview, core.StageReview, ""); err != nil {
		return err
	}
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	if current.SetupContract.Name != "" {
		cfg = cfg.WithSetup(current.SetupContract)
	}
	return d.createReviewRound(ctx, cfg, current, cfg.Routing.Stages[string(core.StageReview)])
}

// ReadMergeReadiness resolves the gate-facing PR state with bounded backoff.
// UNKNOWN is an ordinary pending result and never creates merge.failed noise.
func (d *Dispatcher) ReadMergeReadiness(ctx context.Context, task core.Task) (MergeReadiness, error) {
	var result MergeReadiness
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return result, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return result, err
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || repo.GitHub == "" {
		return result, fmt.Errorf("repository %q does not configure GitHub", current.Repo)
	}
	var pr github.PullRequest
	for attempt, delay := 0, 100*time.Millisecond; attempt < 3; attempt, delay = attempt+1, delay*2 {
		pr, err = d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
		if err != nil {
			if category := github.ErrorCategory(err); category != "" {
				return result, fmt.Errorf("%s: %w", category, err)
			}
			return result, err
		}
		if pr.Mergeable != "UNKNOWN" {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	result = MergeReadiness{State: pr.Mergeable, HeadSHA: pr.HeadSHA, URL: pr.URL, Number: pr.Number}
	approved := current.ApprovedHeadSHA
	if approved == "" {
		approved = current.ReviewedHeadSHA
	}
	if result.State == "UNKNOWN" {
		return result, nil
	}
	if result.State == "CONFLICTING" {
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA); err != nil {
			return result, err
		}
		if !current.MergeApproval {
			_, err = d.dispatchConflictFixLocked(ctx, current, pr, cfg)
		}
		return result, err
	}
	if err = d.clearMergeConflictEpisode(ctx, current, result.State, pr.HeadSHA); err != nil {
		return result, err
	}
	if approved != "" && pr.HeadSHA != "" && approved != pr.HeadSHA {
		if err = d.beginRefreshLocked(ctx, current, pr.HeadSHA, "head-changed", false); err != nil {
			return result, err
		}
		result.State = "STALE"
	}
	return result, nil
}

type mergeConflictEpisode struct {
	ApprovedHead string `json:"approved_head"`
	NewHead      string `json:"new_head"`
}

// currentMergeConflictEpisode reads the append-only task history while the
// caller holds the task lock. A cleared readiness or a terminal conflict-fix
// workflow ends the prior episode; ordinary polling and active-order events do
// not. This keeps GET-driven readiness checks observationally idempotent.
func (d *Dispatcher) currentMergeConflictEpisode(ctx context.Context, taskID string) (mergeConflictEpisode, bool, error) {
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		return mergeConflictEpisode{}, false, err
	}
	terminalJobs := make(map[string]bool)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Kind {
		case "merge.conflict_cleared":
			return mergeConflictEpisode{}, false, nil
		case "approval.stale":
			var payload struct {
				ReasonCode string `json:"reason_code"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ReasonCode == "merge-conflict" {
				return mergeConflictEpisode{}, false, nil
			}
		case "work_order.updated":
			var order core.WorkOrder
			if json.Unmarshal(event.Payload, &order) == nil && (order.State == core.WorkOrderSubmitted || order.State == core.WorkOrderCompleted || order.State == core.WorkOrderTimedOut || order.State == core.WorkOrderStale) {
				terminalJobs[event.JobID] = true
			}
		case "work_order.timed_out", "work_order.stale", "work_order.expired":
			terminalJobs[event.JobID] = true
		case "merge.conflict_fix_dispatched":
			if terminalJobs[event.JobID] {
				return mergeConflictEpisode{}, false, nil
			}
		case "merge.blocked":
			var episode mergeConflictEpisode
			if err := json.Unmarshal(event.Payload, &episode); err != nil {
				return mergeConflictEpisode{}, false, fmt.Errorf("decode merge conflict episode: %w", err)
			}
			return episode, true, nil
		}
	}
	return mergeConflictEpisode{}, false, nil
}

func (d *Dispatcher) recordMergeConflictBlocked(ctx context.Context, task core.Task, approvedHead, newHead string) error {
	episode, active, err := d.currentMergeConflictEpisode(ctx, task.ID)
	if err != nil {
		return err
	}
	if active && episode.ApprovedHead == approvedHead && episode.NewHead == newHead {
		return nil
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": approvedHead, "new_head": newHead})})
}

func (d *Dispatcher) clearMergeConflictEpisode(ctx context.Context, task core.Task, readiness, head string) error {
	episode, active, err := d.currentMergeConflictEpisode(ctx, task.ID)
	if err != nil || !active {
		return err
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.conflict_cleared", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "readiness": readiness, "observed_head": head})})
}

func (d *Dispatcher) DispatchConflictFix(ctx context.Context, task core.Task) (core.WorkOrder, error) {
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || repo.GitHub == "" {
		return core.WorkOrder{}, fmt.Errorf("repository %q does not configure GitHub", current.Repo)
	}
	pr, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return core.WorkOrder{}, err
	}
	// The forge operation above is observational. Durable conflict dispatch
	// enters the transaction-locked command plane without holding a session
	// advisory lock, preventing lock-order inversion with River.
	return d.dispatchConflictFixLocked(ctx, current, pr, cfg)
}

func (d *Dispatcher) dispatchConflictFixLocked(ctx context.Context, current core.Task, pr github.PullRequest, cfg *config.Config) (core.WorkOrder, error) {
	active, ok, err := d.activeImplementationWorkOrder(ctx, current.ID, "merge-conflict")
	if err != nil {
		return core.WorkOrder{}, err
	}
	if ok {
		return active, nil
	}
	if pr.Mergeable != "CONFLICTING" {
		return core.WorkOrder{}, fmt.Errorf("pull request is not conflicting (%s)", pr.Mergeable)
	}
	if current.ApprovedHeadSHA == "" {
		head := current.ReviewedHeadSHA
		if head == "" {
			head = pr.HeadSHA
		}
		if err = d.Store.BindTaskApproval(ctx, current.ID, head); err != nil {
			return core.WorkOrder{}, err
		}
		current.ApprovedHeadSHA = head
	}
	systemCtx := store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem})
	intervention := core.Intervention{TaskID: current.ID, ActorID: "system", ActorRole: core.ActorSystem, Action: core.InterventionRedirect, ReasonCode: "merge-conflict", Comment: "Merge the base branch into the task branch, resolve conflicts, validate, push, and submit for refresh review."}
	if err = d.Store.CreateIntervention(systemCtx, intervention); err != nil {
		return core.WorkOrder{}, err
	}
	if err = d.transition(systemCtx, current.ID, core.TaskConflictDispatch, core.StageImplement, ""); err != nil {
		return core.WorkOrder{}, err
	}
	current, err = d.Store.GetTask(systemCtx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if current.SetupContract.Name != "" {
		cfg = cfg.WithSetup(current.SetupContract)
	}
	if err = d.createWorkOrder(systemCtx, cfg, current, cfg.Routing.Stages[string(core.StageImplement)], "merge-conflict"); err != nil {
		return core.WorkOrder{}, err
	}
	orders, err := d.Store.ListTaskWorkOrders(systemCtx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	var result core.WorkOrder
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].ReasonCode == "merge-conflict" {
			result = orders[i]
			break
		}
	}
	if err = d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, JobID: result.JobID, Kind: "merge.conflict_fix_dispatched", Payload: core.JSONPayload(map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": current.ApprovedHeadSHA, "new_head": pr.HeadSHA, "work_order_id": result.ID})}); err != nil {
		return core.WorkOrder{}, err
	}
	return result, nil
}

// MergeApprovedTask performs the final human-gate transition. A durable-store
// task lock serializes browser retries across control-plane instances; the
// authoritative pre-merge read makes retries after a process restart safe by
// reconciling a PR that GitHub already merged (spec §4, §13.2).
func (d *Dispatcher) MergeApprovedTask(ctx context.Context, task core.Task) error {
	return d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		return d.mergeApprovedTaskLocked(lockedCtx, task)
	})
}

func (d *Dispatcher) mergeApprovedTaskLocked(ctx context.Context, task core.Task) error {
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.State == core.TaskMerged {
		return nil
	}
	if current.State != core.TaskApproved {
		return fmt.Errorf("task %s is not approved for merge", task.ID)
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return d.recordMergeFailure(ctx, current, "workspace_config_unavailable", fmt.Errorf("load workspace repository configuration: %w", err))
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return d.recordMergeFailure(ctx, current, "unsupported_repository", fmt.Errorf("repository %q does not configure GitHub; merge it in its forge and retry reconciliation", current.Repo))
	}

	pr, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		if errors.Is(err, github.ErrPullRequestNotFound) {
			return d.recordMergeFailure(ctx, current, "missing_pull_request", fmt.Errorf("no pull request found for branch %s; push it and submit it for review before merging: %w", current.Branch, err))
		}
		return d.recordMergeFailure(ctx, current, "pull_request_lookup_failed", fmt.Errorf("could not read the pull request for branch %s; verify GitHub authentication and retry: %w", current.Branch, err))
	}
	if pr.Merged {
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL, "result": "already_merged"})}); err != nil {
			return err
		}
		return d.confirmTaskMerged(ctx, current.ID)
	}
	if pr.State != "open" {
		return d.recordMergeFailure(ctx, current, "pull_request_not_open", fmt.Errorf("pull request %s#%d is %s without a merge; reopen or replace it and retry", repo.GitHub, pr.Number, pr.State))
	}
	approvedHead := current.ApprovedHeadSHA
	if approvedHead == "" {
		approvedHead = current.ReviewedHeadSHA
	}
	if approvedHead == "" && pr.HeadSHA != "" {
		// One-time compatibility binding for tasks approved before §21.30.
		approvedHead = pr.HeadSHA
		if err = d.Store.BindTaskApproval(ctx, current.ID, approvedHead); err != nil {
			return err
		}
	}
	if pr.Mergeable == "UNKNOWN" {
		return fmt.Errorf("pull request %s#%d merge readiness is still pending", repo.GitHub, pr.Number)
	}
	if pr.Mergeable == "CONFLICTING" {
		if err = d.recordMergeConflictBlocked(ctx, current, approvedHead, pr.HeadSHA); err != nil {
			return err
		}
		if !current.MergeApproval {
			_, err = d.dispatchConflictFixLocked(ctx, current, pr, cfg)
			return err
		}
		return fmt.Errorf("pull request %s#%d has merge conflicts; dispatch the conflict fix", repo.GitHub, pr.Number)
	}
	if err = d.clearMergeConflictEpisode(ctx, current, pr.Mergeable, pr.HeadSHA); err != nil {
		return err
	}
	if approvedHead != "" && pr.HeadSHA != "" && approvedHead != pr.HeadSHA {
		if err = d.beginRefreshLocked(ctx, current, pr.HeadSHA, "head-changed", false); err != nil {
			return err
		}
		return fmt.Errorf("approval is stale: reviewed head %s differs from current head %s; refresh review dispatched", approvedHead, pr.HeadSHA)
	}
	if pr.Mergeable != "MERGEABLE" {
		return d.recordMergeFailure(ctx, current, "pull_request_not_mergeable", fmt.Errorf("pull request %s#%d is not mergeable (%s); update the branch or resolve required checks and retry", repo.GitHub, pr.Number, pr.Mergeable))
	}
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL})}); err != nil {
		return err
	}
	if err := d.RequestMerge(ctx, repo.GitHub, pr.Number); err != nil {
		return d.recordMergeFailure(ctx, current, "forge_merge_failed", fmt.Errorf("GitHub could not merge pull request %s#%d; resolve required checks or branch protection and retry: %w", repo.GitHub, pr.Number, err))
	}
	confirmed, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return d.recordMergeFailure(ctx, current, "merge_verification_failed", fmt.Errorf("GitHub accepted the merge request but Conveyor could not verify pull request %s#%d; inspect it and retry reconciliation: %w", repo.GitHub, pr.Number, err))
	}
	if !confirmed.Merged {
		return d.recordMergeFailure(ctx, current, "merge_unconfirmed", fmt.Errorf("GitHub did not confirm pull request %s#%d as merged; inspect checks or merge-queue status and retry", repo.GitHub, pr.Number))
	}
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": confirmed.Number, "url": confirmed.URL})}); err != nil {
		return err
	}
	return d.confirmTaskMerged(ctx, current.ID)
}

func (d *Dispatcher) confirmTaskMerged(ctx context.Context, taskID string) error {
	// The task transition atomically resumes dependent queue clocks. Worker
	// polling discovers the now-claimable order; the old durable-queue nudge
	// was a no-op because the existing stage order already owns dispatch.
	return d.transition(ctx, taskID, core.TaskMergeConfirm, "", "")
}

func (d *Dispatcher) recordMergeFailure(ctx context.Context, task core.Task, reason string, mergeErr error) error {
	payload := map[string]any{"reason_code": reason, "error": mergeErr.Error()}
	if category := github.ErrorCategory(mergeErr); category != "" {
		payload["forge_error_category"] = category
	}
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.failed", Payload: core.JSONPayload(payload)}); err != nil {
		return fmt.Errorf("%v; record merge failure: %w", mergeErr, err)
	}
	return mergeErr
}

// PollGitHub preserves issue intake while execution ownership moves to MCP.
func (d *Dispatcher) PollGitHub(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (d *Dispatcher) pollOnce(ctx context.Context) {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return
	}
	for _, repo := range cfg.Repos {
		if repo.GitHub == "" {
			continue
		}
		issues, err := github.ListReadyIssues(ctx, repo.GitHub)
		if err != nil {
			log.Printf("poll %s: %v", repo.GitHub, err)
			continue
		}
		for _, issue := range issues {
			source := fmt.Sprintf("github:%s#%d", repo.GitHub, issue.Number)
			tasks, _ := d.Store.ListTasks(ctx)
			exists := false
			for _, task := range tasks {
				if task.Source == source {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			id := core.NewTaskID()
			task := core.Task{ID: id, Workspace: cfg.Workspace, Source: source, Title: issue.Title, Body: issue.Body, Level: core.L2, Repo: repo.Name, BaseBranch: repo.Base, Branch: gitx.BranchName(id), State: core.TaskClaiming, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
			if err = d.Store.CreateTask(ctx, task); err != nil {
				continue
			}
			if err = github.MarkIssueDispatched(ctx, repo.GitHub, issue.Number, id); err != nil {
				continue
			}
			_, _ = taskops.New(d.Store).Perform(ctx, id, taskops.Command{Kind: core.TaskIntakeFinalize})
			d.Enqueue(ctx, id)
		}
	}
}

// PollOnce runs one explicit-workspace GitHub intake pass.
func (d *Dispatcher) PollOnce(ctx context.Context) { d.pollOnce(ctx) }

func PRBody(task core.Task, evidence ...core.Artifact) string {
	body := fmt.Sprintf("<!-- conveyor:task-link -->\nConveyor task `%s`\n\nSource: %s\n", task.ID, task.Source)
	if task.GitHub != nil && task.GitHub.IssueNumber > 0 {
		body += fmt.Sprintf("\nCloses #%d\n", task.GitHub.IssueNumber)
	}
	if len(evidence) > 0 {
		body += "\n<!-- conveyor:verification-evidence -->\n### Verification evidence\n\n"
		for _, artifact := range evidence {
			if !artifact.EligibleVerificationEvidence() || artifact.TaskID != task.ID {
				continue
			}
			name := strings.NewReplacer("`", "'", "\r", " ", "\n", " ").Replace(strings.TrimSpace(artifact.Name))
			body += fmt.Sprintf("- `%s` — `%s`, %d bytes, SHA-256 `%s`\n", name, artifact.ContentType, artifact.SizeBytes, artifact.ID)
		}
		body += "\nEvidence media remains in Conveyor's task-scoped artifact store. This PR mirror intentionally publishes durable metadata only—no control-plane credentials or private artifact URLs.\n"
	}
	return body
}

func (d *Dispatcher) queueApprovedIssue(ctx context.Context, task core.Task, spec core.SpecVersion) error {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return nil
	}
	sourceNumber, err := sourceIssueNumber(repo.GitHub, task.Source)
	if err != nil {
		return err
	}
	return d.Store.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{
		TaskID: task.ID, Repository: repo.GitHub, SpecVersion: spec.Version,
		Source: task.Source, SourceIssueNumber: sourceNumber,
	})
}

// ReconcileGitHubLifecycles repairs the narrow approval-to-outbox gap after a
// process restart. Remote side effects remain owned by the durable River job.
func (d *Dispatcher) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, task := range tasks {
		spec, ok, getErr := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if getErr != nil {
			return repaired, getErr
		}
		if !ok || !spec.Approved {
			continue
		}
		if _, exists, getErr := d.Store.GetGitHubLifecycle(ctx, task.ID); getErr != nil {
			return repaired, getErr
		} else if exists {
			continue
		}
		if err = d.queueApprovedIssue(ctx, task, spec); err != nil {
			return repaired, err
		}
		if _, exists, getErr := d.Store.GetGitHubLifecycle(ctx, task.ID); getErr != nil {
			return repaired, getErr
		} else if exists {
			repaired++
		}
	}
	return repaired, nil
}

func sourceIssueNumber(repository, source string) (int, error) {
	slug, rawNumber := "", ""
	if value, ok := strings.CutPrefix(strings.TrimSpace(source), "github:"); ok {
		slug, rawNumber, _ = strings.Cut(value, "#")
	} else if parsed, err := url.Parse(strings.TrimSpace(source)); err == nil && strings.EqualFold(parsed.Host, "github.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 4 && parts[2] == "issues" {
			slug, rawNumber = parts[0]+"/"+parts[1], parts[3]
		}
	}
	if slug == "" || rawNumber == "" {
		return 0, nil
	}
	if slug != repository {
		return 0, fmt.Errorf("GitHub source repository %q does not match configured repository %q", slug, repository)
	}
	number, err := strconv.Atoi(rawNumber)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid GitHub source issue %q", source)
	}
	return number, nil
}

// ComposeReviewOutput keeps the §4.1 validator authoritative for MCP input.
func ComposeReviewOutput(review pipeline.Review) string {
	data, _ := json.Marshal(review)
	return "```conveyor:review\n" + string(data) + "\n```"
}
