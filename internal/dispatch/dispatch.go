// Package dispatch advances the durable pipeline. Triage/spec execute inside
// conveyord; implementation and MCP-first review pause at leased work orders
// claimed by operator-owned agents (design-system-architecture; DEC-3).
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
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ErrReviewedHeadUnavailable marks an approval conflict that the operator can
// resolve by publishing and reviewing a concrete task head (design-git-delivery).
var ErrReviewedHeadUnavailable = errors.New("reviewed head SHA is unavailable")

type Dispatcher struct {
	Store                store.Store
	Cfg                  *config.Config
	Pack                 *pack.Bundle
	Agent                inprocess.Agent
	ConfigProvider       func(context.Context) (*config.Config, error)
	PublishIssue         func(context.Context, github.IssuePublication) (github.IssuePublicationResult, error)
	PublishReview        func(context.Context, github.ReviewPublication) (github.ReviewPublicationResult, error)
	ViewPullRequest      func(context.Context, string, string) (github.PullRequest, error)
	RequestMerge         func(context.Context, string, int) error
	ListPullRequestFiles func(context.Context, string, int) ([]string, error)
	ObserveDesignMerge   func(context.Context, monitor.Observation, string) error
	// ReviewDiff resolves the pushed task branch's diff against its base for
	// the in-process review fallback, which has no checkout of its own
	// (design-system-architecture). Injectable for tests.
	ReviewDiff         func(context.Context, *config.Config, core.Task) (string, error)
	ReviewChangedPaths func(context.Context, *config.Config, core.Task) ([]string, error)
	Now                func() time.Time
	memoryQueue        chan queuedTask
	durableQueue       bool
}

func New(st store.Store, cfg *config.Config, agent inprocess.Agent) *Dispatcher {
	return &Dispatcher{
		Store: st, Cfg: cfg, Agent: agent, memoryQueue: make(chan queuedTask, 64), durableQueue: st.IsDurable(),
		PublishIssue:         github.PublishIssue,
		PublishReview:        github.PublishReview,
		ViewPullRequest:      github.PullRequestForBranch,
		RequestMerge:         github.MergePullRequest,
		ListPullRequestFiles: github.PullRequestFiles,
		ReviewDiff:           reviewBranchDiff,
		ReviewChangedPaths:   ReviewBranchChangedPaths,
		Now:                  func() time.Time { return time.Now().UTC() },
	}
}

// ReviewBranchChangedPaths reads filenames from the same pushed branch/base
// comparison that supplies the review patch.
func ReviewBranchChangedPaths(ctx context.Context, cfg *config.Config, task core.Task) ([]string, error) {
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return nil, fmt.Errorf("repository %q is not configured", task.Repo)
	}
	return gitx.NewManager(cfg.CacheDir, "").BranchChangedPaths(ctx, repo.URL, task.Branch, task.BaseBranch)
}

// reviewBranchDiff reads the branch diff from the shared bare cache; the
// implementing agent has already pushed the task branch to origin by the time
// review dispatches (design-git-delivery).
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
	// model attachment (design-system-architecture).
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
// completed review instead of a polling instruction (design-260805-973cd4).
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
		// Its children own the implement orders.
		return nil
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	if task.NextStage == "" {
		return nil
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
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
	// in flight when the execution contract changed (design-harness-execution).
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
		active := order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed
		if reasonCode == "merge-conflict" {
			active = core.WorkOrderActiveForConflictDispatch(order)
		}
		if active {
			return order, true, nil
		}
	}
	return core.WorkOrder{}, false, nil
}

func (d *Dispatcher) createReviewRound(ctx context.Context, cfg *config.Config, task core.Task, route config.StageRoute) error {
	if _, err := store.ServedRequirementsForTask(ctx, d.Store, task.ID, config.ServedRequirementAuthorityNodes(cfg)); err != nil {
		return d.failAuthorityBudget(ctx, task, err)
	}
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
	// remains queued until the first seat claim issues order.claim (design-260805-973cd4).
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

// BuildReviewRound freezes only review-seat shape and timeout policy. Harness,
// model, and effort are resolved by the claimant's local execution setup.
func BuildReviewRound(cfg *config.Config, task core.Task, route config.StageRoute, round int) ([]core.Job, []core.WorkOrder, error) {
	if cfg == nil || round <= 0 {
		return nil, nil, fmt.Errorf("review round configuration and positive round are required")
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
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
		_ = harness
		jobs = append(jobs, core.Job{ID: jobID, TaskID: task.ID, Stage: core.StageReview, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending})
		orders = append(orders, core.WorkOrder{
			ID: jobID, TaskID: task.ID, JobID: jobID, Stage: core.StageReview,
			State: core.WorkOrderQueued, Claimable: true,
			ReviewRound: round, ReviewSeat: seatNumber,
			ReviewKind: func() string {
				if task.ApprovalStale {
					return "refresh"
				}
				return ""
			}(),
			ReviewScope: task.RefreshReviewScope, BaselineSHA: task.RefreshBaselineSHA, HeadSHA: task.RefreshHeadSHA,
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
// (design-harness-execution; DEC-7).
func BuildFutureWorkOrderRouting(cfg *config.Config, task core.Task, stage core.Stage) (core.WorkOrder, error) {
	if cfg == nil || (stage != core.StageSpec && stage != core.StageImplement) {
		return core.WorkOrder{}, fmt.Errorf("future work routing requires spec or implementation stage")
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
	}
	route, ok := cfg.Routing.Stages[string(stage)]
	if !ok || route.Execution != config.ExecutionMCP {
		return core.WorkOrder{}, fmt.Errorf("%s does not use an MCP execution route", stage)
	}
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	now := time.Now().UTC()
	return core.WorkOrder{Stage: stage, ExecutionTimeoutText: route.TimeoutText,
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
	if _, err := store.ServedRequirementsForTask(ctx, d.Store, task.ID, config.ServedRequirementAuthorityNodes(cfg)); err != nil {
		return d.failAuthorityBudget(ctx, task, err)
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
	job := core.Job{ID: jobID, TaskID: task.ID, Stage: task.NextStage, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	now := time.Now().UTC()
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	order := core.WorkOrder{
		ID: jobID, TaskID: task.ID, JobID: jobID, Stage: task.NextStage,
		State: core.WorkOrderQueued, Claimable: true,
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
	}
	if reasonCode != "" {
		payload["reason_code"] = reasonCode
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
	route.Model = config.ResolveControlPlaneModel(string(task.NextStage), route.Model)
	now := time.Now().UTC()
	job := core.Job{ID: jobID, TaskID: task.ID, Stage: task.NextStage, Harness: "openai-responses", ModelTier: route.Model, AuthMode: "deployment-key", Runner: "in-process", Confinement: "control-plane", State: core.JobRunning, StartedAt: now}
	ctx = context.WithValue(ctx, lineageContextMemoKey{}, map[string]lineageContextMemoEntry{})
	input, err := d.buildStageInput(ctx, cfg, task.NextStage, task)
	if err != nil {
		attachmentCount, attachmentTypes := d.modelInputArtifactSummary(ctx, cfg, task)
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
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.dispatched", Payload: core.JSONPayload(map[string]any{
		"stage": task.NextStage, "execution": "in_process", "model": route.Model,
	})}); err != nil {
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
	return d.completeOutput(ctx, cfg, task, job, result.Output, "in-process", reviewAuthority{requirements: input.ServedRequirementSnapshot, governance: input.GovernanceSnapshot})
}

func (d *Dispatcher) modelInputArtifactSummary(ctx context.Context, cfg *config.Config, task core.Task) (int, []string) {
	lineage, err := d.lineageContext(ctx, cfg, task.ID)
	if err != nil {
		return 0, nil
	}
	artifacts := lineage.Artifacts
	types := []string{}
	for _, artifact := range artifacts {
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
	servedAuthority, err := store.ServedRequirementsForTask(ctx, d.Store, task.ID, config.ServedRequirementAuthorityNodes(cfg))
	if err != nil {
		return inprocess.Input{}, d.failAuthorityBudget(ctx, task, err)
	}
	servedRequirements := servedAuthority.Requirements
	input := inprocess.Input{}
	if stage == core.StageReview {
		input.ServedRequirementSnapshot = append([]core.ServedRequirementContext{}, servedRequirements...)
	}
	role = pack.WithRequirementCitationContract(role, stage, servedRequirements)
	if stage == core.StageImplement || stage == core.StageReview {
		governance, governanceErr := store.GovernanceForTask(ctx, d.Store, task.ID, task.Repo)
		if governanceErr != nil {
			return inprocess.Input{}, governanceErr
		}
		if stage == core.StageReview {
			pinned := governance
			input.GovernanceSnapshot = &pinned
		}
		role = pack.WithGovernanceContract(role, stage, governance)
	}
	var prompt strings.Builder
	prompt.WriteString(role)
	fmt.Fprintf(&prompt, "\n\n# Task %s: %s\n\nPlan approval: %t · Merge approval: %t · Repository: %s\n\n%s\n\nBranch: %s (base %s).\n", task.ID, task.Title, task.SpecApproval, task.MergeApproval, task.Repo, task.Body, task.Branch, task.BaseBranch)
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
		spec, exists, getErr := store.ApprovedExecutionDocument(ctx, d.Store, task)
		if getErr != nil {
			return inprocess.Input{}, getErr
		}
		if exists {
			fmt.Fprintf(&prompt, "\n# Approved specification v%d\n\n%s\n", spec.Version, spec.Content)
			prompt.WriteString(pack.DoneCriteriaContract(stage, spec.Content, task.Body, len(servedRequirements) > 0))
		} else {
			prompt.WriteString(pack.DoneCriteriaContract(stage, "", task.Body, len(servedRequirements) > 0))
		}
	}
	if stage == core.StageReview {
		// The in-process reviewer has no checkout, so the change under review
		// must travel in the prompt itself; a missing or oversized diff fails
		// before model execution instead of degrading to a diff-less review
		// (design-system-architecture).
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
		input.OutputSchema = &inprocess.OutputSchema{Name: "conveyor_plan", Schema: pipeline.StructuredPlanSchema()}
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
	lineage, err := d.lineageContext(ctx, cfg, task.ID)
	if err != nil {
		return inprocess.Input{}, fmt.Errorf("assemble lineage context for task %s: %w", task.ID, err)
	}
	prompt.WriteString(lineagecontext.RenderUntrusted(lineage))
	artifacts := lineage.Artifacts
	seen := map[string]bool{}
	totalBytes := 0
	for _, artifact := range artifacts {
		if !artifact.Role.ModelInputEligible() {
			continue
		}
		if seen[artifact.ID] {
			continue
		}
		seen[artifact.ID] = true
		_, content, getErr := d.Store.GetArtifact(ctx, artifact.ID)
		if getErr != nil {
			return inprocess.Input{}, fmt.Errorf("read context artifact %s for task %s: %w", artifact.ID, task.ID, getErr)
		}
		if len(content) > maxModelAttachmentBytes {
			return inprocess.Input{}, fmt.Errorf("context artifact %s (%s) exceeds the %d-byte model attachment limit", artifact.ID, artifact.Name, maxModelAttachmentBytes)
		}
		kind, kindErr := modelAttachmentKind(artifact)
		if kindErr != nil {
			return inprocess.Input{}, kindErr
		}
		if kind == inprocess.AttachmentImage && len(content) > maxModelImageBytes {
			return inprocess.Input{}, fmt.Errorf("image artifact %s (%s) exceeds the %d-byte image input limit", artifact.ID, artifact.Name, maxModelImageBytes)
		}
		totalBytes += len(content)
		if totalBytes > maxModelFileBytes {
			return inprocess.Input{}, fmt.Errorf("context artifact %s (%s) from %s makes task %s exceed the %d-byte combined model input limit", artifact.ID, artifact.Name, contextArtifactSource(artifact), task.ID, maxModelFileBytes)
		}
		fmt.Fprintf(&prompt, "\nContext artifact supplied as %s input: %s (%s, %d bytes, id %s)\n", kind, artifact.Name, artifact.ContentType, len(content), artifact.ID)
		input.Attachments = append(input.Attachments, inprocess.Attachment{ID: artifact.ID, Name: artifact.Name, ContentType: artifact.ContentType, Kind: kind, Content: content})
	}
	input.Prompt = prompt.String()
	return input, nil
}

type lineageContextMemoKey struct{}

// The memo is installed for one synchronous dispatch call and is never shared
// with worker goroutines; buildStageInput and its summary reads are serial.
type lineageContextMemoEntry struct {
	result lineagecontext.Result
	err    error
}

func (d *Dispatcher) lineageContext(ctx context.Context, cfg *config.Config, taskID string) (result lineagecontext.Result, resultErr error) {
	if memo, ok := ctx.Value(lineageContextMemoKey{}).(map[string]lineageContextMemoEntry); ok {
		if cached, exists := memo[taskID]; exists {
			return cached.result, cached.err
		}
		defer func() { memo[taskID] = lineageContextMemoEntry{result: result, err: resultErr} }()
	}
	return lineagecontext.Assemble(ctx, d.Store, cfg, []core.LineageNode{{Type: core.LineageTask, ID: taskID}}, taskID, false)
}

func contextArtifactSource(artifact core.Artifact) string {
	switch {
	case artifact.TaskID != "":
		return "task " + artifact.TaskID
	case artifact.RequirementID != "":
		return "requirement " + artifact.RequirementID
	case artifact.PlanningSessionID != "":
		return "planning session " + artifact.PlanningSessionID
	default:
		return "its lineage source"
	}
}

// modelAttachmentKind is the provider boundary for in-process pipeline context:
// text/documents use Responses input_file, images use input_image, and audio is
// transcribed before the stage request. Anything outside that documented set
// fails before model execution instead of degrading to metadata (design-system-architecture).
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

type reviewAuthority struct {
	requirements []core.ServedRequirementContext
	governance   *core.GovernanceSnapshot
}

func (d *Dispatcher) completeOutput(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, output, reviewer string, authorities ...reviewAuthority) error {
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
		if err = d.recordRequirementSuggestion(ctx, task, result.RequirementID, core.RequirementServesTriage); err != nil {
			return err
		}
		if result.Route == "parked" {
			return d.transition(ctx, task.ID, core.TaskTriagePark, "", core.StageTriage)
		}
		next := core.StageImplement
		// Triage is code-blind classify-and-frame only. The intake-frozen task
		// policy alone selects the next stage (REQ-3, AC-3.1; DEC-17).
		if (task.PolicyVersion > 0 && task.SpecApproval) || (task.PolicyVersion == 0 && (task.Level == core.L2 || task.Level == core.L3)) {
			next = core.StageSpec
		}
		return d.transition(ctx, task.ID, core.TaskStageAdvance, next, "")
	case core.StageSpec:
		result, err := pipeline.RenderStructuredPlan(output)
		if err != nil {
			return invalid(err)
		}
		return d.completeSpec(ctx, task, result, reviewer, job.ModelTier)
	case core.StageReview:
		result, err := pipeline.ParseReview(output)
		if err != nil {
			return invalid(err)
		}
		var authority reviewAuthority
		if len(authorities) > 0 {
			authority = authorities[0]
		}
		return d.applyReview(ctx, cfg, task, job, result, reviewer, job.ID, "", job.ModelTier, authority.requirements, authority.governance, false, invalid)
	default:
		return fmt.Errorf("unsupported in-process stage %s", job.Stage)
	}
}

// ApplyExternalReviewPinned completes an MCP review against the immutable
// citation authority stored on its claimed work order.
func (d *Dispatcher) ApplyExternalReviewPinned(ctx context.Context, task core.Task, job core.Job, result pipeline.Review, reviewWorkOrderID, session, model string, servedRequirements []core.ServedRequirementContext, governance *core.GovernanceSnapshot, claimAuthorized ...bool) error {
	if servedRequirements == nil {
		return fmt.Errorf("review work order %s predates pinned served-requirement authority; release and reclaim it through the current server", reviewWorkOrderID)
	}
	if governance == nil {
		return fmt.Errorf("review work order %s predates pinned governance authority; release and reclaim it through the current server", reviewWorkOrderID)
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
	}
	return d.applyReview(ctx, cfg, task, job, result, "external-mcp", reviewWorkOrderID, session, model, servedRequirements, governance, len(claimAuthorized) > 0 && claimAuthorized[0], nil)
}

// ApplyExternalPlan validates an MCP-authored execution plan and enters the
// exact existing spec-version gate/auto-approval path. Stage identity and
// lifecycle events intentionally remain unchanged.
func (d *Dispatcher) ApplyExternalPlan(ctx context.Context, task core.Task, job core.Job, value pipeline.StructuredPlan, agent, model string) (core.SpecVersion, error) {
	result, err := pipeline.ParsePlan(value.Markdown, value.Decomposition)
	if err != nil {
		return core.SpecVersion{}, err
	}
	return d.completeSpecVersion(ctx, task, result, agent, model)
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
	if err := d.Store.ApproveSpecVersion(ctx, task.ID, version.Version); err != nil {
		return core.SpecVersion{}, err
	}
	if err = d.queueApprovedIssue(ctx, task, version); err != nil {
		return core.SpecVersion{}, err
	}
	if err = d.transition(ctx, task.ID, core.TaskStageAdvance, core.StageImplement, ""); err != nil {
		return core.SpecVersion{}, err
	}
	return version, nil
}

func (d *Dispatcher) applyReview(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, result pipeline.Review, reviewer, reviewWorkOrderID, session, model string, servedRequirements []core.ServedRequirementContext, governance *core.GovernanceSnapshot, claimAuthorized bool, invalid func(error) error) error {
	if servedRequirements == nil {
		servedAuthority, err := store.ServedRequirementsForTask(ctx, d.Store, task.ID, config.ServedRequirementAuthorityNodes(cfg))
		if err != nil {
			return d.failAuthorityBudget(ctx, task, err)
		}
		servedRequirements = servedAuthority.Requirements
	}
	if err := validateReviewCitations(&result, servedRequirements); err != nil {
		if invalid != nil {
			return invalid(err)
		}
		return err
	}
	// Done-criteria authority needs no separate review-order snapshot: approved
	// spec versions are immutable, and legacy children pin their exact parent
	// version on the task. Resolving through the same helper used by prompt
	// construction therefore has migration-064/067 stability without another
	// persisted copy.
	approved, hasApproved, planErr := store.ApprovedExecutionDocument(ctx, d.Store, task)
	if planErr != nil {
		return planErr
	}
	hasPlan := hasApproved && pack.HasExecutionPlan(approved.Content)
	if err := validateDoneCriteriaCoverage(&result, hasPlan); err != nil {
		if invalid != nil {
			return invalid(err)
		}
		return err
	}
	if governance == nil {
		live, err := store.GovernanceForTask(ctx, d.Store, task.ID, task.Repo)
		if err != nil {
			return err
		}
		governance = &live
	}
	if err := validateGovernanceAssessment(*governance, &result); err != nil {
		if invalid != nil {
			return invalid(err)
		}
		return err
	}
	if reviewWorkOrderID == "" {
		reviewWorkOrderID = job.ID
	}
	if model == "" {
		model = job.ModelTier
	}
	var evidenceIDs []string
	if artifacts, artifactErr := d.Store.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}); artifactErr == nil {
		for _, artifact := range artifacts {
			if artifact.TaskID == task.ID && artifact.EligibleVerificationEvidence() {
				evidenceIDs = append(evidenceIDs, artifact.ID)
			}
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
	reviewedCommitSHA := ""
	if decisionReviewKind == "refresh" {
		if decisionHead == "" {
			return fmt.Errorf("refresh review work order %s has no reviewed head SHA", reviewWorkOrderID)
		}
		reviewedCommitSHA = decisionHead
	} else {
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
	}
	if err := taskops.New(d.Store).AcceptReviewDecision(ctx, core.ReviewDecision{
		TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: reviewWorkOrderID,
		Verdict: result.Verdict, ReasonCode: result.ReasonCode, Summary: result.Summary,
		Feedback: result.Feedback, ReviewedCommitSHA: reviewedCommitSHA, EvidenceIDs: evidenceIDs,
		RequirementCitations: result.RequirementCitations, DoneCriteriaAssessment: result.DoneCriteriaCoverage, GovernanceAssessment: result.GovernanceAssessment, Reviewer: reviewer,
		ReviewerModel: model, ReviewerSession: "distinct", SameModelAsImplementer: same,
		ClaimSession: func() string {
			if claimAuthorized {
				return session
			}
			return ""
		}(),
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

func validateDoneCriteriaCoverage(result *pipeline.Review, hasPlan bool) error {
	assessment := result.DoneCriteriaCoverage
	if assessment == nil {
		if hasPlan {
			return fmt.Errorf("review done_criteria_coverage assessment is required when an execution plan is present")
		}
		result.DoneCriteriaCoverage = &core.DoneCriteriaAssessment{Summary: "No execution plan is available", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}}
		return nil
	}
	if assessment.Applicable != hasPlan {
		return fmt.Errorf("review done_criteria_coverage applicable=%t does not match execution plan present=%t", assessment.Applicable, hasPlan)
	}
	if strings.TrimSpace(assessment.Summary) == "" {
		return fmt.Errorf("review done_criteria_coverage summary is required")
	}
	lists := []struct {
		name  string
		items []string
	}{{"satisfied", assessment.Satisfied}, {"unsatisfied", assessment.Unsatisfied}, {"unverified", assessment.Unverified}, {"conflicts", assessment.Conflicts}}
	if !hasPlan {
		for _, list := range lists {
			if len(list.items) != 0 {
				return fmt.Errorf("review done_criteria_coverage %s must be empty when no execution plan exists", list.name)
			}
		}
		return nil
	}
	seen := map[string]string{}
	for _, list := range lists {
		for _, item := range list.items {
			key := strings.TrimSpace(item)
			if key == "" {
				return fmt.Errorf("review done_criteria_coverage %s contains an empty finding", list.name)
			}
			if prior, exists := seen[key]; exists {
				return fmt.Errorf("review done_criteria_coverage finding %q appears in both %s and %s; the finding lists are disjoint", key, prior, list.name)
			}
			seen[key] = list.name
		}
	}
	return nil
}

func validateReviewCitations(result *pipeline.Review, servedRequirements []core.ServedRequirementContext) error {
	if len(servedRequirements) == 0 && result.RequirementCitations == nil {
		result.RequirementCitations = &core.RequirementCitationAssessment{CitedIDs: []string{}, UnknownIDs: []string{}, UnservedIDs: []string{}, Conflicts: []string{}}
	}
	if result.RequirementCitations == nil {
		return fmt.Errorf("review requirement_citations assessment is required for a task with confirmed served requirements")
	}
	if result.RequirementCitations.Applicable != (len(servedRequirements) > 0) {
		return fmt.Errorf("review requirement_citations applicable=%t does not match confirmed served requirements=%t", result.RequirementCitations.Applicable, len(servedRequirements) > 0)
	}
	if len(servedRequirements) == 0 && (len(result.RequirementCitations.CitedIDs) > 0 || len(result.RequirementCitations.UnknownIDs) > 0 || len(result.RequirementCitations.UnservedIDs) > 0 || len(result.RequirementCitations.Conflicts) > 0) {
		return fmt.Errorf("review requirement_citations findings must be empty when no confirmed serves relation exists")
	}
	servedIDs := map[string]bool{}
	for _, requirement := range servedRequirements {
		for _, statement := range requirement.Statements {
			for _, id := range core.RequirementStatementIDs(statement) {
				servedIDs[id] = true
			}
		}
	}
	for _, id := range result.RequirementCitations.CitedIDs {
		if !servedIDs[id] {
			return fmt.Errorf("review requirement_citations cited id %q is not present in the confirmed served requirement version", id)
		}
	}
	// The finding lists are disjoint classifications of cited IDs: an ID in
	// the pinned served set always belongs in cited_ids, so its presence in
	// unknown_ids or unserved_ids contradicts the contract. unserved_ids is
	// not a ledger of served criteria the diff left unexercised.
	seen := map[string]string{}
	for _, id := range result.RequirementCitations.CitedIDs {
		seen[id] = "cited_ids"
	}
	for _, list := range []struct {
		name string
		ids  []string
	}{{"unknown_ids", result.RequirementCitations.UnknownIDs}, {"unserved_ids", result.RequirementCitations.UnservedIDs}} {
		for _, id := range list.ids {
			if servedIDs[id] {
				return fmt.Errorf("review requirement_citations %s entry %q is present in the pinned served requirement version and belongs in cited_ids", list.name, id)
			}
			if prior, duplicated := seen[id]; duplicated {
				return fmt.Errorf("review requirement_citations id %q appears in both %s and %s; the finding lists are disjoint", id, prior, list.name)
			}
			seen[id] = list.name
		}
	}
	return nil
}

func validateGovernanceAssessment(snapshot core.GovernanceSnapshot, result *pipeline.Review) error {
	governing := make(map[string]bool, len(snapshot.Designs))
	for _, design := range snapshot.Designs {
		governing[design.ID] = true
	}
	confirmed, superseded := map[string]bool{}, map[string]bool{}
	for _, decision := range snapshot.Decisions {
		switch decision.Status {
		case core.DecisionConfirmed:
			confirmed[decision.ID] = true
		case core.DecisionSuperseded:
			superseded[decision.ID] = true
		}
	}
	if len(governing) == 0 && len(snapshot.Decisions) == 0 && result.GovernanceAssessment == nil {
		design, decisions := false, false
		result.GovernanceAssessment = &core.GovernanceAssessment{DesignApplicable: &design, DecisionCitable: &decisions}
	}
	if result.GovernanceAssessment == nil {
		return fmt.Errorf("review governance_assessment is required when pinned System Design or decision authority exists")
	}
	if err := core.NormalizeGovernanceAssessment(result.GovernanceAssessment); err != nil {
		return err
	}
	assessment := result.GovernanceAssessment
	if assessment.UsesLegacyApplicable() {
		decisionCitable := len(confirmed) > 0
		assessment.DecisionCitable = &decisionCitable
	}
	if *assessment.DesignApplicable != (len(governing) > 0) {
		return fmt.Errorf("review governance_assessment design_applicable=%t does not match pinned governing System Design authority=%t", *assessment.DesignApplicable, len(governing) > 0)
	}
	if *assessment.DecisionCitable != (len(confirmed) > 0) {
		return fmt.Errorf("review governance_assessment decision_citable=%t does not match pinned confirmed decision authority=%t", *assessment.DecisionCitable, len(confirmed) > 0)
	}
	if len(governing) == 0 && len(snapshot.Decisions) == 0 && (len(assessment.CitedIDs)+len(assessment.UnknownIDs)+len(assessment.UngovernedIDs)+len(assessment.SupersededIDs)+len(assessment.Conflicts) > 0) {
		return fmt.Errorf("review governance_assessment findings must be empty when the pinned governance authority is empty")
	}
	for _, id := range assessment.CitedIDs {
		if governing[id] || confirmed[id] {
			continue
		}
		return fmt.Errorf("review governance_assessment cited id %q is not confirmed governing authority in the pinned snapshot", id)
	}
	for _, id := range assessment.SupersededIDs {
		if !superseded[id] {
			return fmt.Errorf("review governance_assessment superseded id %q is not a superseded decision in the pinned snapshot", id)
		}
	}
	for _, list := range []struct {
		name string
		ids  []string
	}{{"unknown_ids", assessment.UnknownIDs}, {"ungoverned_ids", assessment.UngovernedIDs}} {
		for _, id := range list.ids {
			if governing[id] || confirmed[id] {
				return fmt.Errorf("review governance_assessment %s entry %q is present in the pinned governing authority and belongs in cited_ids", list.name, id)
			}
			if superseded[id] {
				return fmt.Errorf("review governance_assessment %s entry %q is a pinned superseded decision and belongs in superseded_ids", list.name, id)
			}
		}
	}
	return nil
}

func (d *Dispatcher) bounce(ctx context.Context, cfg *config.Config, taskID, jobID, reason, feedback string) error {
	count, _ := d.Store.CountEvents(ctx, taskID, "pipeline.bounced")
	count++
	// The check-in comparison uses bounces since the last human intervention,
	// not the lifetime count.
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
// It replaces the retired triage.feature_suggested event. The event is the
// durable proposal — links are projections
// of events, and a requirement relation is machinery-suggested and
// human-confirmed, never volunteered as a standing edge by an agent.
func (d *Dispatcher) recordRequirementSuggestion(ctx context.Context, task core.Task, requirementID string, source core.RequirementServesSource) error {
	requirementID = strings.TrimSpace(requirementID)
	if requirementID == "" {
		return nil
	}
	requirements, err := d.Store.ListRequirements(ctx)
	if err != nil {
		return err
	}
	for _, requirement := range requirements {
		if requirement.ID == requirementID {
			_, err = d.Store.ProposeRequirementServes(ctx, task.ID, requirement.ID, source, false)
			return err
		}
	}
	return nil
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
	if destination == core.TaskQueued && task.State == core.TaskRunning && command == core.TaskStageBounce {
		orders, listErr := d.Store.ListTaskWorkOrdersSnapshot(ctx, taskID)
		if listErr != nil {
			return listErr
		}
		now := time.Now().UTC()
		for _, order := range orders {
			if order.State != core.WorkOrderClaimed || !order.LeaseExpiresAt.After(now) {
				continue
			}
			return d.Store.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: order.JobID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
				"from_stage": task.NextStage, "next_stage": task.NextStage, "recovery_stage": task.RecoveryStage, "state": core.TaskRunning,
				"suppressed_command": command, "claim_authority_work_order_id": order.ID,
			})})
		}
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

func (d *Dispatcher) failAuthorityBudget(ctx context.Context, task core.Task, cause error) error {
	var budgetErr *store.AuthorityBudgetError
	if !errors.As(cause, &budgetErr) {
		return cause
	}
	_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "context.authority_budget_exceeded", Payload: core.JSONPayload(map[string]any{
		"reason_code": "authority_budget_exceeded", "budget": "authority_nodes", "limit": budgetErr.Limit,
		"remediation": "raise execution_settings.control_plane.planning.context.authority_nodes and redispatch",
	})})
	current, getErr := d.Store.GetTask(ctx, task.ID)
	if getErr != nil {
		return cause
	}
	if current.State == core.TaskQueued {
		if _, transitionErr := taskops.New(d.Store).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); transitionErr != nil {
			return cause
		}
		current.State = core.TaskRunning
	}
	if current.State == core.TaskRunning {
		_, _ = taskops.New(d.Store).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskJobFail, RecoveryStage: current.NextStage, ProjectStages: true})
	}
	return cause
}

func (d *Dispatcher) HandleIntervention(ctx context.Context, task core.Task, latest core.Job, intervention core.Intervention) error {
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	task = current
	planRevision, planRevisionGate, err := PendingPlanRevisionGate(ctx, d.Store, task.ID)
	if err != nil {
		return err
	}
	if planRevisionGate {
		switch intervention.Action {
		case core.InterventionReject:
			return d.transition(ctx, task.ID, core.TaskInterventionReject, "", "")
		case core.InterventionRedirect:
			switch intervention.ReasonCode {
			case PlanRevisionApprovedReasonCode:
				// A revision approval re-enters the ordinary plan stage and its
				// unchanged approval gate (REQ-2, AC-2.2; REQ-3, AC-3.1).
				if err = d.cancelContestedImplementationOrder(ctx, planRevision.WorkOrderID, planRevision.AttemptID); err != nil {
					return err
				}
				return d.transition(ctx, task.ID, core.TaskInterventionRedirect, core.StageSpec, "")
			case PlanRevisionDeclinedReasonCode:
				// Restore the released implementation order through the existing
				// recovery-direction command instead of minting a parallel order
				// or consuming an automatic retry (REQ-2, AC-2.3).
				cfg, cfgErr := d.currentConfig(ctx)
				if cfgErr != nil {
					return cfgErr
				}
				contestedOrder, getErr := d.Store.GetWorkOrder(ctx, planRevision.WorkOrderID)
				if getErr != nil {
					return getErr
				}
				if contestedOrder.State != core.WorkOrderQueued || !contestedOrder.RetrySuppressed || contestedOrder.LastAttemptID != planRevision.AttemptID {
					return fmt.Errorf("contested work order %s is not awaiting plan-revision recovery", contestedOrder.ID)
				}
				queueTimeout := cfg.WorkOrderQueueTimeout
				if queueTimeout <= 0 {
					queueTimeout = config.DefaultWorkOrderQueueTimeout
				}
				requestID := "plan-revision-declined/" + planRevision.AttemptID
				if err = d.transition(ctx, task.ID, core.TaskInterventionRedirect, core.StageImplement, ""); err != nil {
					return err
				}
				_, err = taskops.ExecuteWorkOrder(ctx, d.Store, task.ID, core.WorkOrderCmdRecover, func(lease taskops.TaskLease) (core.WorkOrder, error) {
					return d.Store.RecoverWorkOrderCommand(ctx, lease, planRevision.WorkOrderID, requestID, intervention.Comment, queueTimeout)
				})
				return err
			default:
				return fmt.Errorf("plan-revision redirect requires reason_code %q or %q", PlanRevisionApprovedReasonCode, PlanRevisionDeclinedReasonCode)
			}
		default:
			return fmt.Errorf("plan-revision gate requires redirect or reject intervention")
		}
	}
	switch intervention.Action {
	case core.InterventionCancel:
		_, err := taskops.New(d.Store).Cancel(ctx, intervention)
		return err
	case core.InterventionReject:
		return d.transition(ctx, task.ID, core.TaskInterventionReject, "", "")
	case core.InterventionApprove:
		spec, specGate, err := d.pendingSpecGate(ctx, task.ID)
		if err != nil {
			return err
		}
		if specGate {
			var children []core.Task
			if spec.LegacyGate {
				children, err = d.Store.ApproveSpecVersionAndMaterialize(ctx, task.ID, spec.Version)
			} else {
				err = d.Store.ApproveSpecVersion(ctx, task.ID, spec.Version)
			}
			if err != nil {
				return err
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
		if strings.TrimSpace(head) == "" {
			return fmt.Errorf("%w for task %s; publish and review a task head before approving", ErrReviewedHeadUnavailable, task.ID)
		}
		if err := d.transition(ctx, task.ID, core.TaskInterventionApproveReview, "", ""); err != nil {
			return err
		}
		return d.Store.BindTaskApproval(ctx, task.ID, head)
	case core.InterventionRedirect:
		target := task.RecoveryStage
		if target == "" {
			target = latest.Stage
		}
		_, redirectSpecGate, gateErr := d.pendingSpecGate(ctx, task.ID)
		if gateErr != nil {
			return gateErr
		}
		if redirectSpecGate {
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

func (d *Dispatcher) cancelContestedImplementationOrder(ctx context.Context, workOrderID, attemptID string) error {
	order, err := d.Store.GetWorkOrder(ctx, workOrderID)
	if err != nil {
		return err
	}
	_, err = taskops.ExecuteWorkOrder(ctx, d.Store, order.TaskID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return d.Store.CancelPlanRevisionWorkOrderCommand(ctx, lease, workOrderID, attemptID)
	})
	return err
}

const (
	// These reason codes distinguish the two plan-revision redirect outcomes
	// without expanding the canonical persisted intervention action set.
	PlanRevisionApprovedReasonCode = "plan-revision-approved"
	PlanRevisionDeclinedReasonCode = "plan-revision-declined"
	PlanRevisionRejectedReasonCode = "plan-revision-rejected"
)

// PlanRevisionGateContext is the durable request that controls an awaiting
// plan-revision decision. The event remains the authority; task state alone is
// ambiguous because all operator gates share TaskAwaiting (REQ-2, AC-2.1).
type PlanRevisionGateContext struct {
	WorkOrderID string
	AttemptID   string
	Rationale   string
	PlanVersion int
}

// PendingPlanRevisionGate recognizes the latest audited gate command and
// returns its matching request payload for HTTP validation and dispatch.
func PendingPlanRevisionGate(ctx context.Context, st store.Store, taskID string) (PlanRevisionGateContext, bool, error) {
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return PlanRevisionGateContext{}, false, err
	}
	if task.State != core.TaskAwaiting {
		return PlanRevisionGateContext{}, false, nil
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		return PlanRevisionGateContext{}, false, err
	}
	gateAt := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "task.state_changed" {
			continue
		}
		var transition struct {
			Command core.TaskCommand `json:"command"`
		}
		if err = json.Unmarshal(events[i].Payload, &transition); err != nil {
			return PlanRevisionGateContext{}, false, fmt.Errorf("decode latest task transition for %s: %w", taskID, err)
		}
		if transition.Command != core.TaskGatePlanRevision {
			return PlanRevisionGateContext{}, false, nil
		}
		gateAt = i
		break
	}
	if gateAt < 0 {
		return PlanRevisionGateContext{}, false, nil
	}
	for i := gateAt - 1; i >= 0; i-- {
		if events[i].Kind != "work_order.plan_revision_requested" {
			continue
		}
		var request struct {
			WorkOrderID string `json:"work_order_id"`
			AttemptID   string `json:"attempt_id"`
			Rationale   string `json:"rationale"`
			PlanVersion int    `json:"plan_version"`
		}
		if err = json.Unmarshal(events[i].Payload, &request); err != nil {
			return PlanRevisionGateContext{}, false, fmt.Errorf("decode plan-revision request for %s: %w", taskID, err)
		}
		if request.WorkOrderID == "" || request.AttemptID == "" || strings.TrimSpace(request.Rationale) == "" || request.PlanVersion < 1 {
			return PlanRevisionGateContext{}, false, fmt.Errorf("plan-revision request for %s is incomplete", taskID)
		}
		return PlanRevisionGateContext{WorkOrderID: request.WorkOrderID, AttemptID: request.AttemptID, Rationale: request.Rationale, PlanVersion: request.PlanVersion}, true, nil
	}
	return PlanRevisionGateContext{}, false, fmt.Errorf("plan-revision gate for %s has no request event", taskID)
}

// pendingSpecGate recognizes the exact lifecycle command that parked the task.
// Spec, merge, and failure-recovery gates share the same awaiting/recovery
// projection, while the audited task.state_changed event preserves their
// distinct commands (design-task-lifecycle).
func (d *Dispatcher) pendingSpecGate(
	ctx context.Context,
	taskID string,
) (core.SpecVersion, bool, error) {
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	if task.State != core.TaskAwaiting {
		return core.SpecVersion{}, false, nil
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	specGate := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "task.state_changed" {
			continue
		}
		var transition struct {
			Command core.TaskCommand `json:"command"`
		}
		if err = json.Unmarshal(events[i].Payload, &transition); err != nil {
			return core.SpecVersion{}, false, fmt.Errorf("decode latest task transition for %s: %w", task.ID, err)
		}
		if transition.Command != core.TaskGateSpec {
			return core.SpecVersion{}, false, nil
		}
		specGate = true
		break
	}
	if !specGate {
		return core.SpecVersion{}, false, nil
	}
	spec, ok, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	if !ok {
		return core.SpecVersion{}, false, nil
	}
	if spec.Approved {
		return core.SpecVersion{}, false, nil
	}
	return spec, true, nil
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
	// A refresh already engaged for this exact head pair and scope must not be
	// re-marked: every re-mark re-runs the recover transition, demoting a task
	// whose refresh round may hold a claimed, deliberating seat — observed
	// live as a once-per-poll demotion loop that no completed verdict could
	// survive. The store repeats this guard atomically for concurrent observers;
	// only a changed head pair or a rebound approval starts a new episode.
	if task.ApprovalStale && task.RefreshBaselineSHA == baseline && task.RefreshHeadSHA == newHead && task.RefreshReviewScope == scope {
		return nil
	}
	created, err := d.Store.MarkTaskApprovalStale(ctx, task.ID, baseline, newHead, scope, reason)
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	if scope == config.RefreshReviewNone && !conflict {
		return d.Store.SkipTaskRefresh(ctx, task.ID, newHead, "clean-update")
	}
	command := core.TaskRefreshReview
	if task.State != core.TaskApproved {
		command = core.TaskRecoverRefresh
	}
	if err := d.transition(ctx, task.ID, command, core.StageReview, ""); err != nil {
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
	if current.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(current.SetupContract)
	}
	return d.createReviewRound(ctx, cfg, current, cfg.Routing.Stages[string(core.StageReview)])
}

// ReadMergeReadiness resolves the gate-facing PR state with bounded backoff.
// UNKNOWN is an ordinary pending result and never creates merge.failed noise.
func (d *Dispatcher) ReadMergeReadiness(ctx context.Context, task core.Task) (MergeReadiness, error) {
	var result MergeReadiness
	err := d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		var lockedErr error
		result, lockedErr = d.readMergeReadinessLocked(lockedCtx, task)
		return lockedErr
	})
	return result, err
}

func (d *Dispatcher) readMergeReadinessLocked(ctx context.Context, task core.Task) (MergeReadiness, error) {
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

type mergeConflictDispatchState struct {
	Failures        int
	NextRetryAt     time.Time
	Exhausted       bool
	RecoveryBlocked bool
}

// currentMergeConflictEpisode reads the append-only task history while the
// caller holds the task lock. Only clearing the conflict or observing a new
// approved/head pair ends the prior episode; terminal attempts remain inside
// the unresolved episode and may be retried under its backoff budget.
func (d *Dispatcher) currentMergeConflictEpisode(ctx context.Context, taskID string) (mergeConflictEpisode, bool, error) {
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		return mergeConflictEpisode{}, false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Kind {
		case "merge.conflict_cleared":
			return mergeConflictEpisode{}, false, nil
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

func sameConflictEpisode(payload json.RawMessage, episode mergeConflictEpisode) bool {
	var candidate mergeConflictEpisode
	return json.Unmarshal(payload, &candidate) == nil && candidate == episode
}

func (d *Dispatcher) conflictDispatchState(ctx context.Context, taskID string, episode mergeConflictEpisode, orders []core.WorkOrder) (mergeConflictDispatchState, error) {
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		return mergeConflictDispatchState{}, err
	}
	state := mergeConflictDispatchState{}
	inside := false
	dispatched := map[string]bool{}
	for _, event := range events {
		switch event.Kind {
		case "merge.conflict_cleared":
			inside, state = false, mergeConflictDispatchState{}
		case "merge.blocked":
			inside = sameConflictEpisode(event.Payload, episode)
			state = mergeConflictDispatchState{}
		case "merge.conflict_fix_dispatched":
			if inside && sameConflictEpisode(event.Payload, episode) {
				var payload struct {
					WorkOrderID string `json:"work_order_id"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID != "" {
					dispatched[payload.WorkOrderID] = true
				}
				state.NextRetryAt = time.Time{}
			}
		case "merge.conflict_dispatch_failed":
			if !inside || !sameConflictEpisode(event.Payload, episode) {
				continue
			}
			var payload struct {
				FailureCount int       `json:"failure_count"`
				NextRetryAt  time.Time `json:"next_retry_at"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				state.Failures++
				state.NextRetryAt = payload.NextRetryAt
			}
		case "merge.conflict_dispatch_exhausted":
			if inside && sameConflictEpisode(event.Payload, episode) {
				state.Exhausted = true
			}
		case "merge.conflict_recovery_blocked":
			if inside && sameConflictEpisode(event.Payload, episode) {
				state.RecoveryBlocked = true
			}
		}
	}
	for _, order := range orders {
		if dispatched[order.ID] && terminalConflictFixAttempt(order) {
			state.Failures++
		}
	}
	return state, nil
}

func terminalConflictFixAttempt(order core.WorkOrder) bool {
	switch order.State {
	case core.WorkOrderCancelled, core.WorkOrderStale, core.WorkOrderTimedOut:
		return true
	case core.WorkOrderQueued, core.WorkOrderCompleted:
		return order.RetrySuppressed || order.LastAttemptOutcome != "" || order.LastFailureMessage != ""
	default:
		return false
	}
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
	// The forge operation above is observational. Re-enter the task lock and
	// refresh state before the durable command so watcher and River paths share
	// one serialized admission decision.
	var result core.WorkOrder
	err = d.Store.WithTaskSideEffectLock(ctx, current.ID, func(lockedCtx context.Context) error {
		var lockedErr error
		current, lockedErr = d.Store.GetTask(lockedCtx, current.ID)
		if lockedErr != nil {
			return lockedErr
		}
		approved := current.ApprovedHeadSHA
		if approved == "" {
			approved = current.ReviewedHeadSHA
		}
		if lockedErr = d.recordMergeConflictBlocked(lockedCtx, current, approved, pr.HeadSHA); lockedErr != nil {
			return lockedErr
		}
		result, lockedErr = d.dispatchConflictFixLocked(lockedCtx, current, pr, cfg)
		return lockedErr
	})
	return result, err
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
	episode, present, err := d.currentMergeConflictEpisode(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if !present {
		approved := current.ApprovedHeadSHA
		if approved == "" {
			approved = current.ReviewedHeadSHA
		}
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA); err != nil {
			return core.WorkOrder{}, err
		}
		episode = mergeConflictEpisode{ApprovedHead: approved, NewHead: pr.HeadSHA}
	}
	orders, err := d.Store.ListTaskWorkOrders(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	events, err := d.Store.ListEvents(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	dispatchState, err := d.conflictDispatchState(ctx, current.ID, episode, orders)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if recovery := store.InterruptedReviewRecoveryNeeded(current, store.CurrentReviewOrders(orders, events), events); recovery != nil {
		if !dispatchState.RecoveryBlocked {
			err = d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.conflict_recovery_blocked", Payload: core.JSONPayload(map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "review_round": recovery.ReviewRound, "error": recovery.Reason})})
		}
		return core.WorkOrder{}, err
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	if dispatchState.Failures >= 3 && !dispatchState.Exhausted {
		payload := map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "failure_count": dispatchState.Failures, "retry_suppressed": true, "error": "conflict-fix replacement budget exhausted"}
		if err = d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_exhausted", Payload: core.JSONPayload(payload)}); err != nil {
			return core.WorkOrder{}, err
		}
		dispatchState.Exhausted = true
	}
	if dispatchState.Exhausted || (!dispatchState.NextRetryAt.IsZero() && now.Before(dispatchState.NextRetryAt)) {
		return core.WorkOrder{}, nil
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
	if current.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(current.SetupContract)
	}
	if _, err = store.ServedRequirementsForTask(systemCtx, d.Store, current.ID, config.ServedRequirementAuthorityNodes(cfg)); err != nil {
		return core.WorkOrder{}, err
	}
	prior, err := d.Store.ListJobs(systemCtx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	attempt := 1
	for _, job := range prior {
		if job.Stage == core.StageImplement {
			attempt++
		}
	}
	route := cfg.Routing.Stages[string(core.StageImplement)]
	jobID := fmt.Sprintf("%s-%s-%d", current.ID, core.StageImplement, attempt)
	job := core.Job{ID: jobID, TaskID: current.ID, Stage: core.StageImplement, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	order := core.WorkOrder{ID: jobID, TaskID: current.ID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, ReasonCode: "merge-conflict", BaselineSHA: current.ApprovedHeadSHA, ExecutionTimeoutText: route.TimeoutText, QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now}
	request := store.ConflictFixRequest{TaskID: current.ID, Job: job, WorkOrder: order, Intervention: intervention, ApprovedHead: current.ApprovedHeadSHA, NewHead: pr.HeadSHA}
	var result store.ConflictFixResult
	_, err = taskops.ExecuteWorkOrder(systemCtx, d.Store, current.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) {
		var commandErr error
		result, commandErr = d.Store.CreateConflictFixCommand(systemCtx, lease, request)
		return result.Created, commandErr
	})
	if errors.Is(err, store.ErrConflictReviewRecovery) {
		if !dispatchState.RecoveryBlocked {
			err = d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_recovery_blocked", Payload: core.JSONPayload(map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "error": err.Error()})})
		}
		return core.WorkOrder{}, err
	}
	if err != nil {
		failures := dispatchState.Failures + 1
		delay := time.Minute << (failures - 1)
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		nextRetry := now.Add(delay)
		payload := map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "failure_count": failures, "next_retry_at": nextRetry, "error": err.Error()}
		if eventErr := d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_failed", Payload: core.JSONPayload(payload)}); eventErr != nil {
			return core.WorkOrder{}, fmt.Errorf("conflict dispatch failed: %v; record retry: %w", err, eventErr)
		}
		if failures >= 3 && !dispatchState.Exhausted {
			payload["retry_suppressed"] = true
			if eventErr := d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_exhausted", Payload: core.JSONPayload(payload)}); eventErr != nil {
				return core.WorkOrder{}, fmt.Errorf("conflict dispatch failed: %v; record exhaustion: %w", err, eventErr)
			}
		}
		return core.WorkOrder{}, err
	}
	if result.Created {
		d.Enqueue(systemCtx, current.ID)
	}
	return result.WorkOrder, nil
}

// MergeApprovedTask performs the final human-gate transition. A durable-store
// task lock serializes browser retries across control-plane instances; the
// authoritative pre-merge read makes retries after a process restart safe by
// reconciling a PR that GitHub already merged (design-git-delivery).
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
		if (current.State != core.TaskQueued && current.State != core.TaskRunning) || current.MergeApproval {
			return fmt.Errorf("task %s is not approved for merge", task.ID)
		}
		recoverable, evidenceErr := d.hasRecoverableApprovedReview(ctx, current)
		if evidenceErr != nil {
			return evidenceErr
		}
		if !recoverable {
			return fmt.Errorf("task %s has no accepted review evidence for merge recovery", task.ID)
		}
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
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL, "base_sha": pr.BaseSHA, "head_sha": pr.HeadSHA, "result": "already_merged"})}); err != nil {
			return err
		}
		d.observeConfirmedMerge(ctx, current, repo.GitHub, pr, "merge.reconciled")
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
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": confirmed.Number, "url": confirmed.URL, "base_sha": confirmed.BaseSHA, "head_sha": confirmed.HeadSHA})}); err != nil {
		return err
	}
	d.observeConfirmedMerge(ctx, current, repo.GitHub, confirmed, "merge.confirmed")
	return d.confirmTaskMerged(ctx, current.ID)
}

// observeConfirmedMerge is deliberately non-gating: merge confirmation stays
// authoritative when GitHub file retrieval or drift evaluation fails. The
// observation commit is the PR head SHA recorded in the causal merge event,
// even when the landed squash or merge-commit SHA differs.
func (d *Dispatcher) observeConfirmedMerge(ctx context.Context, task core.Task, githubRepo string, pr github.PullRequest, eventKind string) {
	if d.ObserveDesignMerge == nil {
		return
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	eventID := int64(0)
	if err == nil {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind == eventKind {
				eventID = events[i].ID
				break
			}
		}
	}
	if err == nil && eventID == 0 {
		err = fmt.Errorf("causal %s event was not readable after append", eventKind)
	}
	var paths []string
	if err == nil {
		paths, err = d.ListPullRequestFiles(ctx, githubRepo, pr.Number)
	}
	if err == nil {
		err = d.ObserveDesignMerge(ctx, monitor.Observation{
			Repository: task.Repo, Kind: monitor.LineagedMerge, OccurrenceID: "pr:" + strconv.Itoa(pr.Number),
			SourceURL: pr.URL, CommitSHA: pr.HeadSHA, PullRequestNumber: pr.Number,
			ChangedPaths: paths, CausalEventID: eventID,
		}, task.ID)
	}
	if err != nil {
		if auditor, ok := d.Store.(interface {
			AuditMonitor(context.Context, string, map[string]any) error
		}); ok {
			_ = auditor.AuditMonitor(ctx, "system_design.drift_evaluation_failed", map[string]any{
				"task_id": task.ID, "merge_event_id": eventID, "repository": task.Repo,
				"github_repository": githubRepo, "pull_request": pr.Number, "reason": err.Error(),
			})
		}
	}
}

func (d *Dispatcher) hasRecoverableApprovedReview(ctx context.Context, task core.Task) (bool, error) {
	if task.ApprovedHeadSHA == "" && task.ReviewedHeadSHA == "" {
		return false, nil
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var payload struct {
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			return false, fmt.Errorf("decode latest review resolution for %s: %w", task.ID, err)
		}
		return payload.Verdict == "approve", nil
	}
	return false, nil
}

// ReconcileMergeReadiness is a level-triggered sweep for accepted review
// evidence whose approved-to-merge edge was interrupted. Every candidate is
// revalidated under the task side-effect lock before a forge mutation.
func (d *Dispatcher) ReconcileMergeReadiness(ctx context.Context) (int, error) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	for _, task := range tasks {
		if task.MergeApproval || task.State == core.TaskMerged || task.State == core.TaskClosed || task.State == core.TaskParked || task.State == core.TaskAwaiting {
			continue
		}
		if task.State != core.TaskApproved {
			recoverable, evidenceErr := d.hasRecoverableApprovedReview(ctx, task)
			if evidenceErr != nil {
				return reconciled, evidenceErr
			}
			if !recoverable {
				continue
			}
		}
		before := task.State
		if err = d.MergeApprovedTask(ctx, task); err != nil {
			after, getErr := d.Store.GetTask(ctx, task.ID)
			if getErr != nil {
				return reconciled, getErr
			}
			if after.ApprovalStale || (after.State == core.TaskQueued && after.State != before) {
				reconciled++
				continue
			}
			return reconciled, err
		}
		after, getErr := d.Store.GetTask(ctx, task.ID)
		if getErr != nil {
			return reconciled, getErr
		}
		if before != after.State || after.State == core.TaskMerged {
			reconciled++
		}
	}
	return reconciled, nil
}

func (d *Dispatcher) confirmTaskMerged(ctx context.Context, taskID string) error {
	// The task transition atomically resumes dependent queue clocks. Worker
	// polling discovers the now-claimable order; the old durable-queue nudge
	// was a no-op because the existing stage order already owns dispatch.
	current, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	command := core.TaskMergeConfirm
	if current.State != core.TaskApproved {
		command = core.TaskMergeRecover
	}
	return d.transition(ctx, taskID, command, "", "")
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
