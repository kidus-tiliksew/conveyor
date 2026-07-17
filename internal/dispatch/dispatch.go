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
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type Dispatcher struct {
	Store           store.Store
	Cfg             *config.Config
	Pack            *pack.Bundle
	Agent           inprocess.Agent
	ConfigProvider  func(context.Context) (*config.Config, error)
	ViewPullRequest func(context.Context, string, string) (github.PullRequest, error)
	RequestMerge    func(context.Context, string, int) error
	queue           chan queuedTask
	durableQueue    bool
}

func New(st store.Store, cfg *config.Config, agent inprocess.Agent) *Dispatcher {
	return &Dispatcher{
		Store: st, Cfg: cfg, Agent: agent, queue: make(chan queuedTask, 64),
		ViewPullRequest: github.PullRequestForBranch,
		RequestMerge:    github.MergePullRequest,
	}
}

type queuedTask struct{ Workspace, TaskID string }

func (d *Dispatcher) Enqueue(ctx context.Context, taskID string) {
	if !d.durableQueue {
		workspace, _ := store.WorkspaceFromContext(ctx)
		d.queue <- queuedTask{Workspace: workspace, TaskID: taskID}
	}
}

// DispatchNow advances one task synchronously. The MCP submit_for_review tool
// uses this when review is configured in-process so its result includes the
// completed review instead of a polling instruction (spec §21.4).
func (d *Dispatcher) DispatchNow(ctx context.Context, taskID string) error {
	return d.runTask(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), taskID)
}

func (d *Dispatcher) UseDurableQueue() { d.durableQueue = true }
func (d *Dispatcher) Run(ctx context.Context) {
	if d.durableQueue {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-d.queue:
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
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.NextStage == "" {
		return nil
	}
	route, ok := cfg.Routing.Stages[string(task.NextStage)]
	if !ok {
		return fmt.Errorf("no route for stage %s", task.NextStage)
	}
	if task.NextStage == core.StageReview && route.Execution == config.ExecutionMCP {
		return d.createReviewRound(ctx, cfg, task, route)
	}
	if task.NextStage == core.StageImplement {
		return d.createWorkOrder(ctx, cfg, task, route)
	}
	return d.runInProcess(ctx, cfg, task, route)
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
	// Durable queue redelivery while the task is already inside the current
	// round must reuse the snapshotted seats rather than creating a new round.
	if task.State == core.TaskRunning && latestRound > 0 {
		return nil
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
				return fmt.Errorf("review seat %d references unavailable harness %q", seatNumber, harness)
			}
		}
		jobs = append(jobs, core.Job{ID: jobID, TaskID: task.ID, Stage: core.StageReview, Harness: "external-mcp", ModelTier: seat.Model, AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending})
		orders = append(orders, core.WorkOrder{
			ID: jobID, TaskID: task.ID, JobID: jobID, Stage: core.StageReview,
			State: core.WorkOrderQueued, Claimable: true, SelfReported: true,
			ReviewRound: round, ReviewSeat: seatNumber, RequiredModel: seat.Model,
			RequiredHarness: harness, RequiredHarnessConfig: harnessConfig,
			QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now,
		})
	}
	if err = d.Store.CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
		return err
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": core.StageReview, "execution": "mcp", "review_round": round, "seat_count": len(orders)})})
}

func reviewHarnessSnapshot(cfg *config.Config, name string) (*core.HarnessSnapshot, bool) {
	for _, harness := range cfg.Harnesses {
		if harness.Name != name {
			continue
		}
		return &core.HarnessSnapshot{
			Name: harness.Name, Command: append([]string(nil), harness.Command...),
			ModelArgs:        append([]string(nil), harness.ModelArgs...),
			ProbeCommand:     append([]string(nil), harness.ProbeCommand...),
			ProbeTimeoutText: harness.ProbeTimeoutText,
		}, true
	}
	return nil, false
}

func (d *Dispatcher) createWorkOrder(ctx context.Context, cfg *config.Config, task core.Task, route config.StageRoute) error {
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
	job := core.Job{ID: jobID, TaskID: task.ID, Stage: task.NextStage, Harness: "external-mcp", ModelTier: route.Model, AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	if err := d.Store.UpdateTaskState(ctx, task.ID, core.TaskRunning); err != nil {
		return err
	}
	if err := d.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	now := time.Now().UTC()
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	order := core.WorkOrder{ID: jobID, TaskID: task.ID, JobID: jobID, Stage: task.NextStage, State: core.WorkOrderQueued, Claimable: true, SelfReported: true, QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now}
	if err := d.Store.CreateWorkOrder(ctx, order); err != nil {
		return err
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: jobID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": task.NextStage, "execution": "mcp"})})
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
	if err := d.Store.UpdateTaskState(ctx, task.ID, core.TaskRunning); err != nil {
		return err
	}
	if err := d.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	prompt, err := d.buildStagePrompt(ctx, task.NextStage, task)
	if err != nil {
		return err
	}
	stageCtx, cancel := context.WithTimeout(ctx, route.Timeout)
	defer cancel()
	result, runErr := d.Agent.Run(stageCtx, route.Model, prompt)
	job.EndedAt = time.Now().UTC()
	job.TokensIn = result.TokensIn
	job.TokensOut = result.TokensOut
	job.CostUSD = result.CostUSD
	if len(result.Transcript) != 0 {
		sum := sha256.Sum256(result.Transcript)
		id := fmt.Sprintf("%x", sum)
		artifact, artifactErr := d.Store.CreateArtifact(ctx, core.Artifact{ID: id, Workspace: task.Workspace, Name: job.ID + "-transcript.json", ContentType: "application/json", SizeBytes: int64(len(result.Transcript)), TaskID: task.ID}, result.Transcript)
		if artifactErr == nil {
			_ = d.Store.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "artifact://" + artifact.ID, RedactionStats: result.Redactions, CreatedAt: time.Now().UTC()})
		}
	}
	if runErr != nil {
		job.State = core.JobFailed
		_ = d.Store.UpdateJob(ctx, job)
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.failed", Payload: core.JSONPayload(map[string]string{"error": runErr.Error()})})
		return d.transition(ctx, task.ID, core.TaskAwaiting, "", task.NextStage)
	}
	job.State = core.JobDone
	if err := d.Store.UpdateJob(ctx, job); err != nil {
		return err
	}
	return d.completeOutput(ctx, cfg, task, job, result.Output, "in-process")
}

func (d *Dispatcher) buildStagePrompt(ctx context.Context, stage core.Stage, task core.Task) (string, error) {
	role, err := d.Pack.Role(stage)
	if err != nil {
		return "", err
	}
	var prompt strings.Builder
	prompt.WriteString(role)
	fmt.Fprintf(&prompt, "\n\n# Task %s: %s\n\nExecution mode: %s · Spec approval: %t · Merge approval: %t · Repository: %s\n\n%s\n\nBranch: %s (base %s).\n", task.ID, task.Title, task.Mode, task.SpecApproval, task.MergeApproval, task.Repo, task.Body, task.Branch, task.BaseBranch)
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
			return "", getErr
		}
		if exists && spec.Approved {
			fmt.Fprintf(&prompt, "\n# Approved specification v%d\n\n%s\n", spec.Version, spec.Content)
		}
	}
	artifacts, _ := d.Store.ListArtifacts(ctx)
	for _, artifact := range artifacts {
		if artifact.TaskID != task.ID && (task.FeatureID == "" || artifact.FeatureID != task.FeatureID) {
			continue
		}
		fmt.Fprintf(&prompt, "\nContext artifact: %s (%s, %d bytes, id %s)", artifact.Name, artifact.ContentType, artifact.SizeBytes, artifact.ID)
		if strings.HasPrefix(artifact.ContentType, "text/") || artifact.ContentType == "application/json" {
			_, content, getErr := d.Store.GetArtifact(ctx, artifact.ID)
			if getErr == nil && len(content) <= 1<<20 {
				prompt.WriteString("\n\n" + string(content) + "\n")
			}
		}
	}
	return prompt.String(), nil
}

func (d *Dispatcher) completeOutput(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, output, reviewer string) error {
	invalid := func(parseErr error) error {
		kind := string(job.Stage) + ".output_invalid"
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: kind, Payload: core.JSONPayload(map[string]string{"error": parseErr.Error()})}); err != nil {
			return err
		}
		count, _ := d.Store.CountEvents(ctx, task.ID, kind)
		if count >= cfg.MaxBounces {
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", job.Stage)
		}
		return d.transition(ctx, task.ID, core.TaskQueued, job.Stage, "")
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
		d.suggestFeature(ctx, task, result.Summary)
		if task.PolicyVersion == 0 && (task.Level == core.L3 || result.Route == "human") {
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageTriage)
		}
		if result.Route == "parked" {
			return d.transition(ctx, task.ID, core.TaskParked, "", core.StageTriage)
		}
		next := core.StageImplement
		if (task.PolicyVersion > 0 && task.SpecApproval) || (task.PolicyVersion == 0 && task.Level == core.L2) || result.Route == "spec" {
			next = core.StageSpec
		}
		return d.transition(ctx, task.ID, core.TaskQueued, next, "")
	case core.StageSpec:
		result, err := pipeline.ParseSpec(output)
		if err != nil {
			return invalid(err)
		}
		version, err := d.Store.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: result.Markdown, AcceptanceCount: len(result.Acceptance), Acceptance: core.JSONPayload(result.Acceptance), Decomposition: core.JSONPayload(result.Decomposition)})
		if err != nil {
			return err
		}
		if (task.PolicyVersion > 0 && task.SpecApproval) || (task.PolicyVersion == 0 && task.Level == core.L2) {
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageImplement)
		}
		if err = d.Store.ApproveSpecVersion(ctx, task.ID, version.Version); err != nil {
			return err
		}
		return d.transition(ctx, task.ID, core.TaskQueued, core.StageImplement, "")
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
	requiredModel, requiredHarness, enforcement := model, "", "self-reported"
	if order, orderErr := d.Store.GetWorkOrder(ctx, reviewWorkOrderID); orderErr == nil {
		round, seat = order.ReviewRound, order.ReviewSeat
		requiredModel, requiredHarness = order.RequiredModel, order.RequiredHarness
		if order.ModelEnforcement != "" {
			enforcement = order.ModelEnforcement
		}
	}
	if err := d.Store.AcceptReviewDecision(ctx, core.ReviewDecision{
		TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: reviewWorkOrderID,
		Verdict: result.Verdict, ReasonCode: result.ReasonCode, Summary: result.Summary,
		Feedback: result.Feedback, ReviewedCommitSHA: reviewedCommitSHA, Reviewer: reviewer,
		ReviewerModel: model, ReviewerSession: "distinct", SameModelAsImplementer: same,
		ReviewRound: round, ReviewSeat: seat, RequiredModel: requiredModel,
		RequiredHarness: requiredHarness, ModelEnforcement: enforcement,
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
	_ = d.Store.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement", "reason_code": reason, "feedback": feedback, "count": count, "source": "mcp-review"})})
	if count >= cfg.MaxBounces {
		return d.transition(ctx, taskID, core.TaskAwaiting, "", core.StageImplement)
	}
	return d.transition(ctx, taskID, core.TaskQueued, core.StageImplement, "")
}

func (d *Dispatcher) suggestFeature(ctx context.Context, task core.Task, summary string) {
	features, _ := d.Store.ListFeatures(ctx)
	haystack := strings.ToLower(task.Title + " " + task.Body + " " + summary)
	for _, feature := range features {
		if strings.Contains(haystack, strings.ToLower(feature.Name)) {
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "triage.feature_suggested", Payload: core.JSONPayload(map[string]string{"feature_id": feature.ID, "feature_name": feature.Name})})
			return
		}
	}
}

func (d *Dispatcher) transition(ctx context.Context, taskID string, state core.TaskState, next, recovery core.Stage) error {
	if err := d.Store.SetTaskTransition(ctx, taskID, state, next, recovery); err != nil {
		return err
	}
	if state == core.TaskQueued {
		d.Enqueue(ctx, taskID)
	}
	return nil
}

func (d *Dispatcher) HandleIntervention(ctx context.Context, task core.Task, latest core.Job, intervention core.Intervention) error {
	switch intervention.Action {
	case core.InterventionReject:
		return d.transition(ctx, task.ID, core.TaskClosed, "", "")
	case core.InterventionApprove:
		if latest.Stage == core.StageSpec {
			spec, ok, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("task %s has no spec", task.ID)
			}
			if err = d.Store.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
				return err
			}
			return d.transition(ctx, task.ID, core.TaskQueued, core.StageImplement, "")
		}
		return d.transition(ctx, task.ID, core.TaskApproved, "", "")
	case core.InterventionRedirect:
		target := task.RecoveryStage
		if target == "" {
			target = latest.Stage
		}
		if latest.Stage == core.StageReview {
			target = core.StageImplement
		}
		return d.transition(ctx, task.ID, core.TaskQueued, target, "")
	case core.InterventionPull:
		return nil
	}
	return nil
}

// MergeApprovedTask performs the final human-gate transition. A durable-store
// task lock serializes browser retries across control-plane instances; the
// authoritative pre-merge read makes retries after a process restart safe by
// reconciling a PR that GitHub already merged (spec §4, §13.2).
func (d *Dispatcher) MergeApprovedTask(ctx context.Context, task core.Task) error {
	return d.Store.WithTaskLock(ctx, task.ID, func() error {
		return d.mergeApprovedTaskLocked(ctx, task)
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
			return d.recordMergeFailure(ctx, current, "missing_pull_request", fmt.Errorf("no pull request found for branch %s; push it and submit it for review before merging", current.Branch))
		}
		return d.recordMergeFailure(ctx, current, "pull_request_lookup_failed", fmt.Errorf("could not read the pull request for branch %s; verify GitHub authentication and retry: %w", current.Branch, err))
	}
	if pr.Merged {
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL, "result": "already_merged"})}); err != nil {
			return err
		}
		return d.transition(ctx, current.ID, core.TaskMerged, "", "")
	}
	if pr.State != "open" {
		return d.recordMergeFailure(ctx, current, "pull_request_not_open", fmt.Errorf("pull request %s#%d is %s without a merge; reopen or replace it and retry", repo.GitHub, pr.Number, pr.State))
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
	return d.transition(ctx, current.ID, core.TaskMerged, "", "")
}

func (d *Dispatcher) recordMergeFailure(ctx context.Context, task core.Task, reason string, mergeErr error) error {
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": reason, "error": mergeErr.Error()})}); err != nil {
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
			_ = d.Store.UpdateTaskState(ctx, id, core.TaskQueued)
			d.Enqueue(ctx, id)
		}
	}
}

// PollOnce runs one explicit-workspace GitHub intake pass.
func (d *Dispatcher) PollOnce(ctx context.Context) { d.pollOnce(ctx) }

func PRBody(task core.Task) string {
	return fmt.Sprintf("Conveyor task `%s`\n\nSource: %s\n", task.ID, task.Source)
}

// ComposeReviewOutput keeps the §4.1 validator authoritative for MCP input.
func ComposeReviewOutput(review pipeline.Review) string {
	data, _ := json.Marshal(review)
	return "```conveyor:review\n" + string(data) + "\n```"
}
