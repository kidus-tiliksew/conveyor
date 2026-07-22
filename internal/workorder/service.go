// Package workorder implements the Phase 4.7 leased BYOA lifecycle behind
// both the MCP protocol and UI read models.
package workorder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const MaxTranscriptBytes = 4 << 20

type Service struct {
	Store          store.Store
	Dispatcher     *dispatch.Dispatcher
	Pack           *pack.Bundle
	ConfigProvider func(context.Context) (*config.Config, error)
	OpenPR         func(context.Context, string, string, string, string, string) (string, error)
	ReviewTarget   func(context.Context, string, string) (github.ReviewTarget, error)
}

type Context struct {
	Order         core.WorkOrder        `json:"work_order"`
	Task          core.Task             `json:"task"`
	ApprovedSpec  *core.SpecVersion     `json:"approved_spec,omitempty"`
	PriorSpec     *core.SpecVersion     `json:"prior_spec,omitempty"`
	TriageBrief   *pipeline.TriageBrief `json:"triage_brief,omitempty"`
	RolePrompt    string                `json:"role_prompt"`
	BounceHistory []json.RawMessage     `json:"bounce_history,omitempty"`
	PriorFeedback []string              `json:"prior_feedback,omitempty"`
	Artifacts     []ArtifactReference   `json:"artifacts,omitempty"`
	Diff          string                `json:"diff,omitempty"`
}

type ArtifactReference struct {
	core.Artifact
	WorkOrderID string `json:"work_order_id"`
	ReadTool    string `json:"read_tool"`
}

type ArtifactContent struct {
	Artifact core.Artifact `json:"artifact"`
	Encoding string        `json:"encoding"`
	Data     string        `json:"data"`
}

func (s *Service) config(ctx context.Context) (*config.Config, error) {
	if s.ConfigProvider == nil {
		return nil, fmt.Errorf("work-order config unavailable")
	}
	return s.ConfigProvider(ctx)
}

func (s *Service) List(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := s.Store.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	out := orders[:0]
	for _, order := range orders {
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed ||
			order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut {
			out = append(out, order)
		}
	}
	return out, nil
}

func (s *Service) Claim(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	if strings.TrimSpace(claim.SessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(claim.ClientToken) == "" {
		return core.WorkOrder{}, fmt.Errorf("client_token is required")
	}
	if claim.ClaimantID == "" {
		claim.ClaimantID = "mcp-agent"
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	route, ok := cfg.Routing.Stages[string(order.Stage)]
	timeout := route.Timeout
	if order.ExecutionTimeoutText != "" {
		var parseErr error
		timeout, parseErr = time.ParseDuration(order.ExecutionTimeoutText)
		if parseErr != nil || timeout <= 0 {
			return core.WorkOrder{}, fmt.Errorf("work-order execution timeout snapshot is invalid")
		}
	}
	if !ok || timeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order execution timeout unavailable")
	}
	claim.ExecutionTimeout = timeout
	if err = s.enforce(ctx, order); err != nil {
		return core.WorkOrder{}, err
	}
	order, err = s.Store.ClaimWorkOrder(ctx, id, claim)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Service) Redispatch(ctx context.Context, id string) (core.WorkOrder, error) {
	cfg, err := s.config(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	timeout := cfg.WorkOrderQueueTimeout
	if timeout <= 0 {
		timeout = config.DefaultWorkOrderQueueTimeout
	}
	s.refreshQueuedHarnessSnapshot(ctx, cfg, id)
	return s.Store.RedispatchWorkOrder(ctx, id, timeout)
}

func (s *Service) Recover(ctx context.Context, id, requestID string) (core.WorkOrder, error) {
	if strings.TrimSpace(requestID) == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	timeout := cfg.WorkOrderQueueTimeout
	if timeout <= 0 {
		timeout = config.DefaultWorkOrderQueueTimeout
	}
	s.refreshQueuedHarnessSnapshot(ctx, cfg, id)
	return s.Store.RecoverWorkOrder(ctx, id, requestID, timeout)
}

// refreshQueuedHarnessSnapshot re-resolves an operator-recovered order's pinned
// harness snapshot from the current registry before it re-enters the queue
// (spec §21.32). Best-effort: retaining the prior snapshot is the explicit
// fallback, and the recovery transition that follows reports the authoritative
// state errors.
func (s *Service) refreshQueuedHarnessSnapshot(ctx context.Context, cfg *config.Config, id string) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return
	}
	snapshot, changed := core.RefreshedHarnessSnapshot(cfg.Harnesses, order.RequiredHarnessConfig)
	if !changed {
		return
	}
	_, _ = s.Store.RefreshWorkOrderHarnessSnapshot(ctx, id, snapshot)
}

func (s *Service) RecoverInterruptedReviewRound(ctx context.Context, taskID, requestID string) (store.InterruptedReviewRecoveryResult, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery request_id is required")
	}
	if _, err := s.Store.GetTask(ctx, taskID); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	orders, err := s.Store.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	orders = store.CurrentReviewOrders(orders, events)
	recovery := store.InterruptedReviewRecoveryNeeded(orders)
	if recovery == nil {
		// The store owns durable idempotency and may still return the original
		// result after recovered seats have since been claimed.
		recovery = &store.InterruptedReviewRecoveryState{ReviewRound: latestReviewRound(orders)}
	}
	if recovery.ReviewRound == 0 {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: task %s has no interrupted review round", store.ErrReviewRetryConflict, taskID)
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	timeout := cfg.WorkOrderQueueTimeout
	if timeout <= 0 {
		timeout = config.DefaultWorkOrderQueueTimeout
	}
	return s.Store.RecoverInterruptedReviewRound(ctx, store.InterruptedReviewRecoveryRequest{TaskID: taskID, RequestID: requestID, Round: recovery.ReviewRound}, timeout)
}

func latestReviewRound(orders []core.WorkOrder) int {
	latest := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound > latest {
			latest = order.ReviewRound
		}
	}
	return latest
}

// RetryReviewRound starts a new full panel after the latest immutable review
// round terminally timed out. The current PR head and workspace configuration
// are verified before the store performs the atomic/idempotent transition.
func (s *Service) RetryReviewRound(ctx context.Context, taskID, requestID, reason string) (store.ReviewRoundRetryResult, error) {
	requestID = strings.TrimSpace(requestID)
	reason = strings.TrimSpace(reason)
	if requestID == "" || reason == "" {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("review retry request_id and operator reason are required")
	}
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	// Durable audit data also provides a fast idempotent response after the new
	// round becomes active and is no longer itself recovery-eligible.
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_retried" {
			continue
		}
		var prior struct {
			RequestID  string `json:"request_id"`
			Reason     string `json:"reason"`
			PriorRound int    `json:"prior_round"`
			NewRound   int    `json:"new_round"`
			PRHead     string `json:"pr_head"`
		}
		if json.Unmarshal(events[i].Payload, &prior) != nil || prior.RequestID != requestID {
			continue
		}
		if prior.Reason != reason {
			return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrReviewRetryConflict, requestID)
		}
		orders, listErr := s.Store.ListTaskWorkOrders(ctx, taskID)
		if listErr != nil {
			return store.ReviewRoundRetryResult{}, listErr
		}
		var created []core.WorkOrder
		for _, order := range orders {
			if order.Stage == core.StageReview && order.ReviewRound == prior.NewRound {
				created = append(created, order)
			}
		}
		return store.ReviewRoundRetryResult{RequestID: requestID, TaskID: taskID, PriorRound: prior.PriorRound, NewRound: prior.NewRound, PRHead: prior.PRHead, WorkOrders: created}, nil
	}
	orders, err := s.Store.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	recovery := store.ReviewRecoveryNeeded(orders)
	if recovery == nil {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: task %s does not have a terminal timed-out review round", store.ErrReviewRetryConflict, taskID)
	}
	if task.NextStage != core.StageReview {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: task %s requires implementation handoff before another review", store.ErrReviewRetryConflict, taskID)
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	route, ok := cfg.Routing.Stages[string(core.StageReview)]
	if !ok || route.Execution != config.ExecutionMCP {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("review retry requires the current MCP review route")
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("verified PR head is unavailable for repo %s", task.Repo)
	}
	reviewTarget := s.ReviewTarget
	if reviewTarget == nil {
		reviewTarget = github.ReviewTargetForBranch
	}
	target, err := reviewTarget(ctx, repo.GitHub, task.Branch)
	if err != nil {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("verify current PR head: %w", err)
	}
	recordedHead := ""
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var opened struct {
			HeadSHA string `json:"head_sha"`
		}
		if json.Unmarshal(events[i].Payload, &opened) == nil && opened.HeadSHA != "" {
			recordedHead = opened.HeadSHA
			break
		}
	}
	if recordedHead == "" {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("verified implementation handoff PR head is unavailable")
	}
	if target.HeadSHA != recordedHead {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: PR head changed from %s to %s and requires implementation handoff", store.ErrReviewRetryConflict, recordedHead, target.HeadSHA)
	}
	jobs, newOrders, err := dispatch.BuildReviewRound(cfg, task, route, recovery.PriorRound+1)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	return s.Store.RetryReviewRound(ctx, store.ReviewRoundRetryRequest{TaskID: taskID, RequestID: requestID, Reason: reason, PriorRound: recovery.PriorRound, PRHead: target.HeadSHA}, jobs, newOrders)
}

func (s *Service) Get(ctx context.Context, id, session string) (Context, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return Context{}, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return Context{}, err
	}
	role, err := s.Pack.Role(order.Stage)
	if err != nil {
		return Context{}, err
	}
	if order.Stage == core.StageReview {
		role = pack.MCPReviewRole(role)
	}
	if order.Stage == core.StageImplement && order.ReasonCode == "merge-conflict" {
		role += "\n\nThis is a merge-conflict fix order (spec §21.30). Use `conveyor checkout " + task.ID + "`, merge the base branch `" + task.BaseBranch + "` into the task branch `" + task.Branch + "`, resolve every conflict, run the repository validation, push the task branch, and call submit_for_review. Do not rebase or force-push.\n"
	}
	result := Context{Order: order, Task: task, RolePrompt: role}
	if order.Stage == core.StageSpec {
		// Spec work has repository/base context but never receives a branch.
		result.Task.Branch = ""
	}
	if spec, ok, getErr := s.Store.GetLatestSpecVersion(ctx, task.ID); getErr != nil {
		return Context{}, getErr
	} else if ok {
		if order.Stage == core.StageSpec {
			result.PriorSpec = &spec
		} else if spec.Approved {
			result.ApprovedSpec = &spec
		}
	}
	events, _ := s.Store.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			result.BounceHistory = append(result.BounceHistory, event.Payload)
		}
		if event.Kind == "triage.completed" {
			var triage pipeline.Triage
			if json.Unmarshal(event.Payload, &triage) == nil {
				brief := triage.Brief
				result.TriageBrief = &brief
			}
		}
	}
	interventions, _ := s.Store.ListInterventions(ctx, task.ID)
	for _, item := range interventions {
		if item.Action == core.InterventionRedirect && strings.TrimSpace(item.Comment) != "" {
			result.PriorFeedback = append(result.PriorFeedback, item.Comment)
		}
	}
	artifacts, err := s.Store.ListArtifacts(ctx)
	if err != nil {
		return Context{}, fmt.Errorf("list task artifacts: %w", err)
	}
	for _, artifact := range artifacts {
		if artifact.TaskID == task.ID || (task.FeatureID != "" && artifact.FeatureID == task.FeatureID) {
			artifact.DownloadURL = ""
			result.Artifacts = append(result.Artifacts, ArtifactReference{Artifact: artifact, WorkOrderID: order.ID, ReadTool: "read_artifact"})
		}
	}
	if order.Stage == core.StageReview {
		cfg, _ := s.config(ctx)
		if repo, ok := cfg.Repo(task.Repo); ok && repo.GitHub != "" {
			if order.ReviewKind == "refresh" && order.ReviewScope == config.RefreshReviewDelta && order.BaselineSHA != "" && order.HeadSHA != "" {
				result.Diff, _ = github.DiffBetween(ctx, repo.GitHub, order.BaselineSHA, order.HeadSHA)
			} else {
				result.Diff, _ = github.DiffForBranch(ctx, repo.GitHub, task.Branch)
			}
		}
	}
	return result, nil
}

func (s *Service) ReadArtifact(ctx context.Context, id, session, artifactID string) (ArtifactContent, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return ArtifactContent{}, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return ArtifactContent{}, err
	}
	artifact, content, err := s.Store.GetArtifactForContext(ctx, artifactID, task.ID, task.FeatureID)
	if err != nil {
		// Keep unauthorized ownership mismatches indistinguishable from missing
		// artifacts; artifact ids alone are never bearer capabilities (spec §21.4).
		return ArtifactContent{}, fmt.Errorf("artifact %s not found for work order %s", artifactID, id)
	}
	return ArtifactContent{Artifact: artifact, Encoding: "base64", Data: base64.StdEncoding.EncodeToString(content)}, nil
}

func (s *Service) Progress(ctx context.Context, id, session, message string) (core.WorkOrder, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if err = s.enforce(ctx, order); err != nil {
		return core.WorkOrder{}, err
	}
	order.Progress = strings.TrimSpace(message)
	if len(order.Progress) > 4000 {
		order.Progress = order.Progress[:4000]
	}
	order.SelfReported = true
	err = s.Store.UpdateWorkOrder(ctx, order)
	return order, err
}

func (s *Service) Usage(ctx context.Context, id, session string, tokensIn, tokensOut int64, cost float64) (core.WorkOrder, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if tokensIn < 0 || tokensOut < 0 || cost < 0 {
		return core.WorkOrder{}, fmt.Errorf("usage values cannot be negative")
	}
	order.TokensIn = tokensIn
	order.TokensOut = tokensOut
	order.CostUSD = cost
	order.SelfReported = true
	if err = s.Store.UpdateWorkOrder(ctx, order); err != nil {
		return core.WorkOrder{}, err
	}
	job, ok, _ := s.Store.GetLatestJob(ctx, order.TaskID)
	if ok && job.ID == order.JobID {
		job.TokensIn = tokensIn
		job.TokensOut = tokensOut
		job.CostUSD = &cost
		_ = s.Store.UpdateJob(ctx, job)
	}
	return order, nil
}

func (s *Service) UploadTranscript(ctx context.Context, id, session, transcript string) (core.Artifact, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return core.Artifact{}, err
	}
	if len(transcript) > MaxTranscriptBytes {
		return core.Artifact{}, fmt.Errorf("transcript exceeds %d bytes", MaxTranscriptBytes)
	}
	clean, stats := redact.New(nil).Redact(transcript)
	content := []byte(clean)
	sum := sha256.Sum256(content)
	artifactID := fmt.Sprintf("%x", sum)
	task, _ := s.Store.GetTask(ctx, order.TaskID)
	artifact, err := s.Store.CreateArtifact(ctx, core.Artifact{ID: artifactID, Workspace: task.Workspace, Name: order.ID + "-self-reported-transcript.txt", ContentType: "text/plain", SizeBytes: int64(len(content)), Role: core.ArtifactRoleGeneratedAudit, TaskID: task.ID}, content)
	if err != nil {
		return core.Artifact{}, err
	}
	if err = s.Store.UpsertTranscript(ctx, core.Transcript{JobID: order.JobID, URI: "artifact://" + artifact.ID, RedactionStats: stats, CreatedAt: time.Now().UTC()}); err != nil {
		return core.Artifact{}, err
	}
	_ = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "transcript.self_reported", Payload: core.JSONPayload(map[string]any{"artifact_id": artifact.ID, "self_reported": true})})
	return artifact, nil
}

func (s *Service) SubmitForReview(ctx context.Context, id, session string) (map[string]any, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return nil, err
	}
	if order.Stage != core.StageImplement {
		return nil, fmt.Errorf("work order %s is not implement", id)
	}
	if err = s.enforce(ctx, order); err != nil {
		return nil, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return nil, fmt.Errorf("repo %s not found", task.Repo)
	}
	prURL := ""
	reviewedHead := ""
	if repo.GitHub != "" {
		if spec, exists, specErr := s.Store.GetLatestSpecVersion(ctx, task.ID); specErr != nil {
			return nil, specErr
		} else if exists && spec.Approved {
			if task.GitHub == nil {
				return nil, fmt.Errorf("approved task %s has no durable GitHub issue association yet; retry after reconciliation", task.ID)
			}
			if task.GitHub.Repository != repo.GitHub {
				return nil, fmt.Errorf("task GitHub association repository %q does not match configured repository %q", task.GitHub.Repository, repo.GitHub)
			}
			if task.GitHub.State != core.GitHubPublicationPublished || task.GitHub.IssueNumber <= 0 {
				return nil, fmt.Errorf("GitHub issue publication for task %s is %s; retry after publication reconciliation", task.ID, task.GitHub.State)
			}
		}
		openPR := s.OpenPR
		if openPR == nil {
			openPR = github.OpenPRForBranch
		}
		prURL, err = openPR(ctx, repo.GitHub, task.Branch, task.BaseBranch, task.Title, dispatch.PRBody(task))
		if err != nil {
			return nil, fmt.Errorf("open PR: %w", err)
		}
		reviewTarget := s.ReviewTarget
		if reviewTarget == nil {
			reviewTarget = github.ReviewTargetForBranch
		}
		target, targetErr := reviewTarget(ctx, repo.GitHub, task.Branch)
		if targetErr != nil {
			return nil, fmt.Errorf("resolve reviewed PR head: %w", targetErr)
		}
		if err = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"url": prURL, "number": target.Number, "head_sha": target.HeadSHA})}); err != nil {
			return nil, fmt.Errorf("record reviewed PR head: %w", err)
		}
		reviewedHead = target.HeadSHA
	}
	order.State = core.WorkOrderSubmitted
	if err = s.Store.UpdateWorkOrder(ctx, order); err != nil {
		return nil, err
	}
	job, ok, _ := s.Store.GetLatestJob(ctx, task.ID)
	if ok && job.ID == order.JobID {
		job.State = core.JobDone
		job.EndedAt = time.Now().UTC()
		_ = s.Store.UpdateJob(ctx, job)
	}
	if order.ReasonCode == "merge-conflict" {
		baseline := order.BaselineSHA
		if baseline == "" {
			baseline = task.ApprovedHeadSHA
		}
		scope := task.SetupContract.RefreshReview
		if scope == "" || scope == config.RefreshReviewNone {
			scope = config.RefreshReviewDelta
		}
		if err = s.Store.MarkTaskApprovalStale(ctx, task.ID, baseline, reviewedHead, scope, "merge-conflict"); err != nil {
			return nil, err
		}
	} else if task.ApprovalStale && reviewedHead != "" && reviewedHead != task.RefreshHeadSHA {
		// A fix submitted while the approval is stale must retarget the
		// refresh review to the pushed head; each refresh seat order
		// contracts the baseline and the new head (spec §21.30), so leaving
		// the recorded head behind would review a snapshot that predates
		// the fix on every subsequent round.
		if err = s.Store.AdvanceTaskRefreshHead(ctx, task.ID, reviewedHead); err != nil {
			return nil, err
		}
	}
	if err = s.Store.SetTaskTransition(ctx, task.ID, core.TaskQueued, core.StageReview, ""); err != nil {
		return nil, err
	}
	reviewExecution := cfg.Routing.Stages["review"].Execution
	if reviewExecution == config.ExecutionInProcess {
		if err = s.Dispatcher.DispatchNow(ctx, task.ID); err != nil {
			return nil, err
		}
		result, resultErr := s.latestReviewResult(ctx, task.ID, order.UpdatedAt)
		if resultErr != nil {
			return nil, resultErr
		}
		result["pr_url"] = prURL
		result["review_execution"] = reviewExecution
		result["await_review"] = false
		return result, nil
	}
	s.Dispatcher.Enqueue(ctx, task.ID)
	return map[string]any{"pr_url": prURL, "review_execution": reviewExecution, "await_review": true}, nil
}

// SubmitSpec completes a claimed spec order. Validation errors are returned
// directly and leave the order claimed for in-session correction (spec §21.33).
func (s *Service) SubmitSpec(ctx context.Context, id, session string, value pipeline.StructuredSpec) (map[string]any, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return nil, err
	}
	if order.Stage != core.StageSpec {
		return nil, fmt.Errorf("work order %s is not spec", id)
	}
	if err = s.enforce(ctx, order); err != nil {
		return nil, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	jobs, err := s.Store.ListJobs(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var job core.Job
	found := false
	for _, candidate := range jobs {
		if candidate.ID == order.JobID {
			job, found = candidate, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("spec job unavailable")
	}
	version, err := s.Dispatcher.ApplyExternalSpec(ctx, task, job, value, order.Agent, order.Model)
	if err != nil {
		return nil, err
	}
	order.State = core.WorkOrderCompleted
	if err = s.Store.UpdateWorkOrder(ctx, order); err != nil {
		return nil, err
	}
	job.State = core.JobDone
	job.EndedAt = time.Now().UTC()
	job.CostUSD = &order.CostUSD
	job.TokensIn = order.TokensIn
	job.TokensOut = order.TokensOut
	if err = s.Store.UpdateJob(ctx, job); err != nil {
		return nil, err
	}
	current, err := s.Store.GetTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task_id": task.ID, "version": version.Version, "approved": current.State != core.TaskAwaiting, "next_stage": current.NextStage}, nil
}

func (s *Service) AwaitReview(ctx context.Context, id, session string, wait time.Duration) (map[string]any, error) {
	order, err := s.authorizedForAwait(ctx, id, session)
	if err != nil {
		return nil, err
	}
	if order.Stage != core.StageImplement {
		return nil, fmt.Errorf("await_review requires an implement work order")
	}
	if wait <= 0 || wait > 10*time.Minute {
		wait = 5 * time.Minute
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if result, resultErr := s.latestReviewResult(ctx, order.TaskID, order.UpdatedAt); resultErr == nil {
			return result, nil
		} else if resultErr.Error() != "review pending" {
			return nil, resultErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return map[string]any{"status": "pending"}, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) authorizedForAwait(ctx context.Context, id, session string) (core.WorkOrder, error) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if session == "" || order.SessionID != session {
		return core.WorkOrder{}, fmt.Errorf("work order %s belongs to another session", id)
	}
	if order.State == core.WorkOrderSubmitted {
		// Submission makes the review wait durable; the implementation claim
		// lease no longer governs this read-only same-session operation.
		return order, nil
	}
	if order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not awaiting review", id)
	}
	if !order.LeaseExpiresAt.After(time.Now()) {
		return core.WorkOrder{}, fmt.Errorf("work order lease expired")
	}
	return order, nil
}

func (s *Service) latestReviewResult(ctx context.Context, taskID string, after time.Time) (map[string]any, error) {
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if (events[i].Kind == "review.round_completed" || events[i].Kind == "review.completed") && !events[i].At.Before(after) {
			var result map[string]any
			if err = json.Unmarshal(events[i].Payload, &result); err != nil {
				return nil, err
			}
			if events[i].Kind == "review.round_completed" {
				return result, nil
			}
			if _, panelEvent := result["review_round"]; !panelEvent {
				return result, nil
			}
		}
	}
	return nil, fmt.Errorf("review pending")
}

func (s *Service) SubmitVerdict(ctx context.Context, id, session string, review pipeline.Review) (map[string]any, error) {
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return nil, err
	}
	if order.Stage != core.StageReview {
		return nil, fmt.Errorf("work order %s is not review", id)
	}
	if err = s.enforce(ctx, order); err != nil {
		return nil, err
	}
	validated, err := pipeline.ParseReview(dispatch.ComposeReviewOutput(review))
	if err != nil {
		return nil, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	jobs, err := s.Store.ListJobs(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var job core.Job
	found := false
	for _, candidate := range jobs {
		if candidate.ID == order.JobID {
			job, found = candidate, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("review job unavailable")
	}
	if err = s.Dispatcher.ApplyExternalReview(ctx, task, job, validated, order.ID, session, order.Model); err != nil {
		return nil, err
	}
	order.State = core.WorkOrderCompleted
	if err = s.Store.UpdateWorkOrder(ctx, order); err != nil {
		return nil, err
	}
	job.State = core.JobDone
	job.EndedAt = time.Now().UTC()
	job.CostUSD = &order.CostUSD
	job.TokensIn = order.TokensIn
	job.TokensOut = order.TokensOut
	if err = s.Store.UpdateJob(ctx, job); err != nil {
		return nil, err
	}
	result := map[string]any{"verdict": validated.Verdict, "task_id": task.ID, "review_round": order.ReviewRound, "review_seat": order.ReviewSeat, "model_enforcement": order.ModelEnforcement}
	if order.RequiredEffort != "" {
		result["required_effort"] = order.RequiredEffort
	}
	if aggregate, aggregateErr := s.reviewRoundResult(ctx, task.ID, order.ReviewRound); aggregateErr == nil {
		result["round_status"] = "completed"
		result["aggregate"] = aggregate
	} else {
		result["round_status"] = "pending"
	}
	return result, nil
}

func (s *Service) reviewRoundResult(ctx context.Context, taskID string, round int) (map[string]any, error) {
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var result map[string]any
		if json.Unmarshal(events[i].Payload, &result) != nil {
			continue
		}
		if value, ok := result["review_round"].(float64); ok && int(value) == round {
			return result, nil
		}
	}
	return nil, fmt.Errorf("review pending")
}

func (s *Service) authorized(ctx context.Context, id, session string) (core.WorkOrder, error) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if order.State == core.WorkOrderTimedOut {
		return core.WorkOrder{}, store.ErrWorkOrderTimedOut
	}
	if order.State == core.WorkOrderStale {
		return core.WorkOrder{}, store.ErrWorkOrderStale
	}
	if order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimed", id)
	}
	if session == "" || order.SessionID != session {
		return core.WorkOrder{}, fmt.Errorf("work order %s is claimed by another session", id)
	}
	if !order.LeaseExpiresAt.After(time.Now()) {
		return core.WorkOrder{}, fmt.Errorf("work order lease expired")
	}
	return order, nil
}

func (s *Service) enforce(ctx context.Context, order core.WorkOrder) error {
	current, err := s.Store.GetWorkOrder(ctx, order.ID)
	if err != nil {
		return err
	}
	if current.State == core.WorkOrderTimedOut {
		return store.ErrWorkOrderTimedOut
	}
	if current.State == core.WorkOrderStale {
		return store.ErrWorkOrderStale
	}
	return nil
}
