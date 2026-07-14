// Package workorder implements the Phase 4.7 leased BYOA lifecycle behind
// both the MCP protocol and UI read models.
package workorder

import (
	"context"
	"crypto/sha256"
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
	Order         core.WorkOrder    `json:"work_order"`
	Task          core.Task         `json:"task"`
	ApprovedSpec  *core.SpecVersion `json:"approved_spec,omitempty"`
	RolePrompt    string            `json:"role_prompt"`
	BounceHistory []json.RawMessage `json:"bounce_history,omitempty"`
	PriorFeedback []string          `json:"prior_feedback,omitempty"`
	Artifacts     []core.Artifact   `json:"artifacts,omitempty"`
	Diff          string            `json:"diff,omitempty"`
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
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed {
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
	if err = s.enforce(ctx, order); err != nil {
		return core.WorkOrder{}, err
	}
	order, err = s.Store.ClaimWorkOrder(ctx, id, claim)
	if err != nil {
		return core.WorkOrder{}, err
	}
	job, ok, err := s.Store.GetLatestJob(ctx, order.TaskID)
	if err == nil && ok && job.ID == order.JobID {
		job.State = core.JobRunning
		job.ModelTier = claim.Model
		if updateErr := s.Store.UpdateJob(ctx, job); updateErr != nil {
			return core.WorkOrder{}, updateErr
		}
	}
	return order, nil
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
	result := Context{Order: order, Task: task, RolePrompt: role}
	if spec, ok, getErr := s.Store.GetLatestSpecVersion(ctx, task.ID); getErr != nil {
		return Context{}, getErr
	} else if ok && spec.Approved {
		result.ApprovedSpec = &spec
	}
	events, _ := s.Store.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			result.BounceHistory = append(result.BounceHistory, event.Payload)
		}
	}
	interventions, _ := s.Store.ListInterventions(ctx, task.ID)
	for _, item := range interventions {
		if item.Action == core.InterventionRedirect && strings.TrimSpace(item.Comment) != "" {
			result.PriorFeedback = append(result.PriorFeedback, item.Comment)
		}
	}
	artifacts, _ := s.Store.ListArtifacts(ctx)
	for _, artifact := range artifacts {
		if artifact.TaskID == task.ID || (task.FeatureID != "" && artifact.FeatureID == task.FeatureID) {
			artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
			result.Artifacts = append(result.Artifacts, artifact)
		}
	}
	if order.Stage == core.StageReview {
		cfg, _ := s.config(ctx)
		if repo, ok := cfg.Repo(task.Repo); ok && repo.GitHub != "" {
			result.Diff, _ = github.DiffForBranch(ctx, repo.GitHub, task.Branch)
		}
	}
	return result, nil
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
		job.CostUSD = cost
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
	artifact, err := s.Store.CreateArtifact(ctx, core.Artifact{ID: artifactID, Workspace: task.Workspace, Name: order.ID + "-self-reported-transcript.txt", ContentType: "text/plain", SizeBytes: int64(len(content)), TaskID: task.ID}, content)
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
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return nil, fmt.Errorf("repo %s not found", task.Repo)
	}
	prURL := ""
	if repo.GitHub != "" {
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
	s.Dispatcher.Enqueue(task.ID)
	return map[string]any{"pr_url": prURL, "review_execution": reviewExecution, "await_review": true}, nil
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
		if events[i].Kind == "review.completed" && !events[i].At.Before(after) {
			var result map[string]any
			if err = json.Unmarshal(events[i].Payload, &result); err != nil {
				return nil, err
			}
			return result, nil
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
	job, ok, err := s.Store.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || job.ID != order.JobID {
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
	job.CostUSD = order.CostUSD
	job.TokensIn = order.TokensIn
	job.TokensOut = order.TokensOut
	if err = s.Store.UpdateJob(ctx, job); err != nil {
		return nil, err
	}
	return map[string]any{"verdict": validated.Verdict, "task_id": task.ID}, nil
}

func (s *Service) authorized(ctx context.Context, id, session string) (core.WorkOrder, error) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
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
	cfg, err := s.config(ctx)
	if err != nil {
		return err
	}
	route := cfg.Routing.Stages[string(order.Stage)]
	jobs, err := s.Store.ListJobs(ctx, order.TaskID)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.ID == order.JobID && route.Timeout > 0 && time.Now().After(job.StartedAt.Add(route.Timeout)) {
			return fmt.Errorf("work order wall clock exhausted")
		}
	}
	return nil
}
