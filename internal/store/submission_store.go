package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ExecutionDocumentReader lets acceptance resolve the canonical approved plan
// using reads held within the same transaction as the decision.
type ExecutionDocumentReader interface {
	GetApprovedSpecVersion(context.Context, string) (core.SpecVersion, bool, error)
	GetSpecVersion(context.Context, string, int) (core.SpecVersion, bool, error)
}

// ExecutionDocumentLookup reads the newest approved version when version is
// zero, and the exact version otherwise. Backends supply transaction-bound reads.
type ExecutionDocumentLookup func(context.Context, string, int) (core.SpecVersion, bool, error)

func (lookup ExecutionDocumentLookup) GetApprovedSpecVersion(ctx context.Context, id string) (core.SpecVersion, bool, error) {
	return lookup(ctx, id, 0)
}

func (lookup ExecutionDocumentLookup) GetSpecVersion(ctx context.Context, id string, version int) (core.SpecVersion, bool, error) {
	return lookup(ctx, id, version)
}

// ValidateReviewAcceptance enforces plan coverage before any review acceptance
// side effect (REQ-4, req-task-centric-delivery; component-work-orders).
func ValidateReviewAcceptance(ctx context.Context, reader ExecutionDocumentReader, task core.Task, decision *core.ReviewDecision, verification ...VerificationReviewState) error {
	if decision.VerificationAssessment != nil {
		decision.VerificationAssessment.Actor = ActorFromContext(ctx).ID
	}
	if task.SetupContract.VerifyStage {
		if len(verification) != 1 {
			return ErrVerificationState
		}
		if err := ValidateSealedVerificationReview(task, decision, verification[0]); err != nil {
			return err
		}
	}
	approved, exists, err := ApprovedExecutionDocument(ctx, reader, task)
	if err != nil {
		return err
	}
	// Match pack.HasExecutionPlan: legacy blueprint documents remain no-plan
	// cases, while approved markdown execution plans require coverage.
	_, hasDone := pipeline.PlanDoneCriteria(approved.Content)
	_, planErr := pipeline.ParsePlan(approved.Content, nil)
	return ValidateDoneCriteriaCoverage(&decision.DoneCriteriaAssessment, decision.Verdict, exists && hasDone && planErr == nil)
}

// ValidateDoneCriteriaCoverage preserves assessment-shape diagnostics and rejects
// an approval that reports unresolved plan criteria. It never changes findings
// or a verdict; only the supported missing no-plan assessment is normalized.
func ValidateDoneCriteriaCoverage(coverage **core.DoneCriteriaAssessment, verdict string, hasPlan bool) error {
	assessment := *coverage
	if assessment == nil {
		if hasPlan {
			return fmt.Errorf("review done_criteria_coverage assessment is required when an execution plan is present")
		}
		*coverage = &core.DoneCriteriaAssessment{Summary: "No execution plan is available", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}}
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
	if verdict == "approve" {
		var blocking []string
		for _, list := range lists[1:] {
			if len(list.items) > 0 {
				blocking = append(blocking, list.name)
			}
		}
		if len(blocking) > 0 {
			return fmt.Errorf("review done_criteria_coverage blocks approve: unresolved criteria in %s; correct the assessment or submit changes_requested", strings.Join(blocking, ", "))
		}
	}
	return nil
}

// ApprovedExecutionDocument resolves the immutable approved document that
// governs implementation and review. Legacy materialized children inherit the
// exact parent blueprint version that created them; new task-centric work uses
// the task's own approved plan.
func ApprovedExecutionDocument(ctx context.Context, st ExecutionDocumentReader, task core.Task) (core.SpecVersion, bool, error) {
	approved, ok, err := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || ok || task.ParentTaskID == "" || task.OriginSpecVersion < 1 {
		return approved, ok, err
	}
	approved, ok, err = st.GetSpecVersion(ctx, task.ParentTaskID, task.OriginSpecVersion)
	if err != nil {
		return approved, false, err
	}
	if !ok || !approved.Approved {
		return core.SpecVersion{}, false, fmt.Errorf("approved blueprint %s version %d not found for child task %s", task.ParentTaskID, task.OriginSpecVersion, task.ID)
	}
	return approved, true, nil
}

type ConflictFixRequest struct {
	TaskID       string
	Job          core.Job
	WorkOrder    core.WorkOrder
	Intervention core.Intervention
	ApprovedHead string
	NewHead      string
}

// ImplementationSubmission is the exact claimed implement order, carrying its
// submitted state and head, whose job SubmitImplementationCommand completes.
// The command rechecks the live claim and session and completes only the job
// named by Order.JobID, never the task's latest job.
type ImplementationSubmission struct {
	Order   core.WorkOrder
	EndedAt time.Time
}

// ValidateImplementationSubmission checks the request shape shared by every
// backend before any write.
func ValidateImplementationSubmission(request ImplementationSubmission) error {
	order := request.Order
	if order.ID == "" || order.TaskID == "" || order.JobID == "" {
		return fmt.Errorf("implementation submission requires work order, task, and job identifiers")
	}
	if order.Stage != core.StageImplement || order.State != core.WorkOrderSubmitted || strings.TrimSpace(order.HeadSHA) == "" || order.SessionID == "" {
		return fmt.Errorf("implementation submission for %s requires a submitted implement order with session and head", order.ID)
	}
	return nil
}

// ValidateSubmissionJob refuses completion of a missing, foreign, or
// non-running job before any submission write.
func ValidateSubmissionJob(order core.WorkOrder, job core.Job, found bool) error {
	if !found || job.TaskID != order.TaskID {
		return fmt.Errorf("work order %s job %s not found for task %s", order.ID, order.JobID, order.TaskID)
	}
	if job.State != core.JobRunning {
		return fmt.Errorf("work order %s job %s is %s; submission requires a running job", order.ID, job.ID, job.State)
	}
	return ValidateJobTransition(job.State, core.JobDone)
}

type ConflictFixResult struct {
	WorkOrder core.WorkOrder
	Created   bool
}

func (m *memory) approvedExecutionDocumentLocked(task core.Task) (core.SpecVersion, bool) {
	versions := m.specs[task.ID]
	for index := len(versions) - 1; index >= 0; index-- {
		if versions[index].Approved {
			return versions[index], true
		}
	}
	if task.ParentTaskID == "" || task.OriginSpecVersion < 1 {
		return core.SpecVersion{}, false
	}
	for _, plan := range m.specs[task.ParentTaskID] {
		if plan.Version == task.OriginSpecVersion && plan.Approved {
			return plan, true
		}
	}
	return core.SpecVersion{}, false
}

// NonAdvancingRefreshBinding detects an approved refresh verdict that still
// binds the baseline instead of the distinct head carried by the review order.
// Settlement leaves the stale episode intact so readiness polling cannot
// silently create another review round for the same baseline/head/scope.
func NonAdvancingRefreshBinding(decision core.ReviewDecision, approvedHeadSHA string) bool {
	return decision.ReviewKind == "refresh" && decision.BaselineSHA != "" && decision.HeadSHA != "" &&
		decision.BaselineSHA != decision.HeadSHA && approvedHeadSHA == decision.BaselineSHA
}

func (m *memory) CreateConflictFixCommand(ctx context.Context, lease taskops.TaskLease, request ConflictFixRequest) (ConflictFixResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return ConflictFixResult{}, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return ConflictFixResult{}, fmt.Errorf("conflict-fix create requires a valid taskops lease")
	}
	if request.TaskID == "" || request.Job.TaskID != request.TaskID || request.Job.Stage != core.StageImplement ||
		request.WorkOrder.ID != request.Job.ID || request.WorkOrder.JobID != request.Job.ID ||
		request.WorkOrder.TaskID != request.TaskID || request.WorkOrder.Stage != core.StageImplement ||
		request.WorkOrder.ReasonCode != "merge-conflict" || request.Intervention.TaskID != request.TaskID ||
		request.Intervention.Action != core.InterventionRedirect || request.Intervention.ReasonCode != "merge-conflict" {
		return ConflictFixResult{}, fmt.Errorf("invalid conflict-fix command for task %s", request.TaskID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[request.TaskID]
	if !ok {
		return ConflictFixResult{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && task.Workspace != selected {
		return ConflictFixResult{}, fmt.Errorf("task %s belongs to workspace %s, not %s", request.TaskID, task.Workspace, selected)
	}
	var taskOrders []core.WorkOrder
	for _, existing := range m.workOrders {
		if existing.TaskID != request.TaskID {
			continue
		}
		taskOrders = append(taskOrders, existing)
		if existing.Stage == core.StageImplement && existing.ReasonCode == "merge-conflict" && core.WorkOrderActiveForConflictDispatch(existing) {
			return ConflictFixResult{WorkOrder: existing}, nil
		}
	}
	if InterruptedReviewRecoveryNeeded(task, CurrentReviewOrders(taskOrders, m.events[request.TaskID]), m.events[request.TaskID]) != nil {
		return ConflictFixResult{}, ErrConflictReviewRecovery
	}
	state, transitionCommand, err := core.TransitionConflictDispatch(task.State)
	if err != nil {
		return ConflictFixResult{}, err
	}
	if _, exists := m.workOrders[request.WorkOrder.ID]; exists {
		return ConflictFixResult{}, fmt.Errorf("work order %s already exists", request.WorkOrder.ID)
	}
	if _, _, exists := m.findJobLocked(request.Job.ID); exists {
		return ConflictFixResult{}, fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, request.Job.ID)
	}
	now := time.Now().UTC()
	intervention := request.Intervention
	actor := ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = now
	}
	order := request.WorkOrder
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	if order.QueueEnteredAt.IsZero() {
		order.QueueEnteredAt = order.CreatedAt
	}
	if order.QueueDeadline.IsZero() {
		order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
	}
	order.State, order.Claimable, order.UpdatedAt = core.WorkOrderQueued, true, now
	if m.taskBlockedLocked(order.TaskID) {
		order.QueueBlockedAt, order.Claimable = order.QueueEnteredAt, false
	}

	fromState, fromStage := task.State, task.NextStage
	task.State, task.NextStage, task.RecoveryStage = state, core.StageImplement, ""
	m.tasks[task.ID] = task
	m.nextReviewID++
	intervention.ID = m.nextReviewID
	m.interventions[task.ID] = append(m.interventions[task.ID], intervention)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "intervention.redirect", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}), At: intervention.At})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": transitionCommand})})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": core.StageImplement, "recovery_stage": "", "state": state})})
	m.jobs[task.ID] = append(m.jobs[task.ID], request.Job)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: request.Job.ID, Kind: "job.created", Payload: core.JSONPayload(request.Job)})
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: request.Job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: request.Job.ID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": core.StageImplement, "execution": "mcp", "reason_code": "merge-conflict"})})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: request.Job.ID, Kind: "merge.conflict_fix_dispatched", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": request.ApprovedHead, "new_head": request.NewHead, "work_order_id": order.ID})})
	return ConflictFixResult{WorkOrder: order, Created: true}, nil
}

// SubmitImplementationCommand validates the exact job before any write, so
// the memory backend's single mutation lock gives the same all-or-nothing
// result as the SQL transactions.
func (m *memory) SubmitImplementationCommand(ctx context.Context, lease taskops.TaskLease, request ImplementationSubmission) (core.Job, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Job{}, err
	}
	if err := ValidateImplementationSubmission(request); err != nil {
		return core.Job{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	order := request.Order
	job, index, found := m.findJobLocked(order.JobID)
	if err := ValidateSubmissionJob(order, job, found); err != nil {
		return core.Job{}, err
	}
	if current, ok := m.workOrders[order.ID]; !ok || current.TaskID != order.TaskID || current.JobID != order.JobID || current.Stage != core.StageImplement {
		return core.Job{}, fmt.Errorf("work order %s does not match its submission", order.ID)
	}
	if err := m.updateWorkOrderCommandLocked(ctx, lease, order, core.WorkOrderCmdSubmitForReview); err != nil {
		return core.Job{}, err
	}
	ended := request.EndedAt.UTC()
	if ended.IsZero() {
		ended = time.Now().UTC()
	}
	job.State, job.EndedAt = core.JobDone, ended
	m.jobs[job.TaskID][index] = job
	m.appendEventLocked(ctx, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "job.updated", Payload: core.JSONPayload(job)})
	return job, nil
}

func (m *memory) AttachSubmissionGovernance(ctx context.Context, taskID, repository string, changedPaths []string, attribution SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	if _, err := RequireActor(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	workspace := workspaceOrDefault(ctx, "")
	contextWorkspace, workspaceScoped := WorkspaceFromContext(ctx)
	if !ok || (workspaceScoped && task.Workspace != contextWorkspace) {
		return nil, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if core.TaskTerminal(task.State) {
		return nil, ErrTaskTerminal
	}
	designs := make([]core.GovernanceDesignContext, 0)
	for key, document := range m.systemDesigns {
		if key.workspace != workspace || document.CurrentVersion < 1 || document.Archived {
			continue
		}
		versions := m.systemDesignVersions[key]
		if document.CurrentVersion > len(versions) {
			return nil, fmt.Errorf("system design %s current version %d is unavailable", document.ID, document.CurrentVersion)
		}
		version := versions[document.CurrentVersion-1]
		designs = append(designs, core.GovernanceDesignContext{ID: document.ID, Title: document.Title, Category: document.Category, Version: version.Version, Content: version.Content, Governs: append([]core.GovernedScope(nil), version.Governs...)})
	}
	_, active := ActiveTaskContextReferences(m.events[taskID])
	attached := make([]core.TaskDesignContext, 0)
	now := time.Now().UTC()
	for _, match := range core.ResolveGovernedDesigns(designs, repository, changedPaths) {
		if active[match.Design.ID] > 0 {
			continue
		}
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{
			"id": match.Design.ID, "version": match.Design.Version, "source": "submission_diff",
			"work_order_id": attribution.WorkOrderID, "session_id": attribution.SessionID, "matching_paths": match.MatchingPaths,
		})})
		active[match.Design.ID] = match.Design.Version
		attached = append(attached, core.TaskDesignContext{ID: match.Design.ID, Title: match.Design.Title, Version: match.Design.Version})
	}
	return attached, nil
}

func (m *memory) BindTaskApproval(ctx context.Context, id, headSHA string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return fmt.Errorf("approved head SHA is required")
	}
	task.ReviewedHeadSHA, task.ApprovedHeadSHA, task.ApprovalStale = headSHA, headSHA, false
	task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = "", "", ""
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "approval.bound", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": id, "approved_head": headSHA})})
	return nil
}

func (m *memory) MarkTaskApprovalStale(ctx context.Context, id, approvedHeadSHA, newHeadSHA, scope, reason string) (bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s not found", id)
	}
	if approvedHeadSHA == "" || newHeadSHA == "" || approvedHeadSHA == newHeadSHA {
		return false, fmt.Errorf("distinct approved and new head SHAs are required")
	}
	if task.ApprovalStale && task.RefreshBaselineSHA == approvedHeadSHA && task.RefreshHeadSHA == newHeadSHA {
		return false, nil
	}
	task.ApprovedHeadSHA, task.ApprovalStale = approvedHeadSHA, true
	task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = approvedHeadSHA, newHeadSHA, scope
	m.supersedeVerificationLocked(ctx, id, newHeadSHA)
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "approval.stale", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": id, "reason_code": reason, "approved_head": approvedHeadSHA, "new_head": newHeadSHA, "review_scope": scope})})
	return true, nil
}

func (m *memory) AdvanceTaskRefreshHead(ctx context.Context, id, newHeadSHA string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	newHeadSHA = strings.TrimSpace(newHeadSHA)
	if newHeadSHA == "" {
		return fmt.Errorf("new head SHA is required")
	}
	if !task.ApprovalStale {
		return fmt.Errorf("task %s has no stale approval to refresh", id)
	}
	if task.RefreshHeadSHA == newHeadSHA {
		return nil
	}
	prior := task.RefreshHeadSHA
	task.RefreshHeadSHA = newHeadSHA
	m.supersedeVerificationLocked(ctx, id, newHeadSHA)
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "review.refresh_head_advanced", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": id, "approved_head": task.RefreshBaselineSHA, "prior_head": prior, "new_head": newHeadSHA, "review_scope": task.RefreshReviewScope})})
	return nil
}

func (m *memory) SkipTaskRefresh(ctx context.Context, id, newHeadSHA, reason string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	if task.SetupContract.VerifyStage {
		return fmt.Errorf("verify-stage tasks cannot skip refresh verification")
	}
	baseline := task.ApprovedHeadSHA
	task.ReviewedHeadSHA, task.ApprovedHeadSHA, task.ApprovalStale = newHeadSHA, newHeadSHA, false
	task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = "", "", ""
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "review.refresh_skipped", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": id, "reason_code": reason, "approved_head": baseline, "new_head": newHeadSHA})})
	return nil
}
