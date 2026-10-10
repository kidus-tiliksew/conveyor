// Package store holds event-sourced control-plane state behind an interface.
// The memory implementation is for unit tests and explicit local development;
// Postgres is the durable implementation (component-persistence).
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

var (
	ErrWorkspaceRequired = errors.New("workspace context is required")
	// ErrNotFound classifies a missing durable planning resource independently
	// of the backing store, so callers can distinguish model-supplied bad IDs
	// from unavailable infrastructure.
	ErrNotFound                      = errors.New("resource not found")
	ErrDispatchJobConflict           = errors.New("dispatch job already exists")
	ErrReferenceDocumentNameConflict = errors.New("reference document name already exists")
	ErrLastWorkspaceOperator         = errors.New("cannot revoke the sole workspace operator; grant another operator first")
	ErrWorkspaceConflict             = errors.New("workspace id or name already exists")
	ErrRequirementSlugConflict       = errors.New("requirement slug already exists")
	ErrSystemDesignIDConflict        = errors.New("system design id already exists")
	ErrSystemDesignSlugConflict      = errors.New("system design slug already exists")
	ErrDecisionIDConflict            = errors.New("decision id already exists")
	ErrDecisionSupersessionConflict  = errors.New("decision supersession conflicts with current state")
	ErrDecisionSweepTransition       = errors.New("invalid decision supersession sweep transition")
	ErrTaskContextProposalTransition = errors.New("invalid task context proposal transition")
	// ErrRequirementServesTransition is the compatibility name retained for
	// callers migrated onto the unified task-context proposal lifecycle.
	ErrRequirementServesTransition = ErrTaskContextProposalTransition
	ErrWorkOrderStale              = errors.New("work order is stale and requires redispatch")
	ErrWorkOrderTimedOut           = errors.New("work order execution deadline exceeded")
	ErrReviewRetryConflict         = errors.New("review round retry conflicts with current state")
	ErrPairingInvalid              = errors.New("worker pairing token is invalid, expired, or already used")
	ErrWorkerUnauthorized          = errors.New("worker credential is invalid or revoked")
	ErrWorkOrderCancelled          = errors.New("work order was cancelled")
	ErrWorkOrderPreempted          = errors.New("work order was preempted by an operator")
	ErrWorkOrderPreemptConflict    = errors.New("work order preempt conflict")
	ErrTaskTerminal                = errors.New("task is already terminal")
	// ErrTaskBranchConflict reports that another open task in the same
	// workspace repository already holds the assigned branch
	// (req-task-branch-assignment AC-4.1).
	ErrTaskBranchConflict = errors.New("open task branch already assigned")
	// ErrInvalidBranch reports an empty or illegal git branch assignment
	// (req-task-branch-assignment REQ-2).
	ErrInvalidBranch = errors.New("invalid_branch")
	// ErrBranchNotAttachable reports a base branch or another task's
	// default conveyor/task-<id> assignment (AC-2.11, AC-2.12).
	ErrBranchNotAttachable = errors.New("branch_not_attachable")
	// ErrWorkOrderClaimed refuses remap while any work order is claimed (AC-2.8).
	ErrWorkOrderClaimed = errors.New("work_order_claimed")
	// ErrPullRequestRecorded refuses remap after pull_request.opened (AC-2.7).
	ErrPullRequestRecorded      = errors.New("pull_request_recorded")
	ErrTaskDependencyCycle      = errors.New("task dependency would create a cycle")
	ErrTaskDependencyConflict   = errors.New("task dependency request conflicts with current state")
	ErrLineageRebuildValidation = errors.New("invalid lineage rebuild request")
	ErrLineageRebuildConflict   = errors.New("lineage rebuild request conflicts with a prior request")
	// ErrWorkOrderClaimLost is the order-scoped counterpart to
	// ErrWorkerUnauthorized: the caller's credential is valid but the order is
	// no longer claimed by it, typically because the claim lease expired and
	// ownership returned to the queue. Kept distinct so agents do
	// not misdiagnose a lapsed claim as a revoked credential.
	ErrWorkOrderClaimLost = errors.New("work order claim is no longer held by this worker (claim expired or order reassigned)")
	// ErrWorkOrderReleasedAtCheckpoint reports that renewal observed an exact
	// same-session claim which already released itself for operator direction.
	// The release remains authoritative; renewal does not resurrect the claim.
	ErrWorkOrderReleasedAtCheckpoint = errors.New("work_order_released_checkpoint: work order claim was released by this session at an operator checkpoint")
	// ErrWorkOrderClaimUnauthorized distinguishes a live claim owned by a
	// different authenticated claimant from an expired or reassigned lease.
	ErrWorkOrderClaimUnauthorized = errors.New("authenticated caller does not hold this live work order claim")
	// ErrVerificationEvidenceClaimConflict intentionally collapses every
	// missing, stale, expired, superseded, stage-mismatched, or differently
	// owned claim into one response so the upload route cannot disclose another
	// task's ownership.
	ErrVerificationEvidenceClaimConflict = errors.New("verification evidence upload requires the matching live implement claim")
)

// WorkspaceControlStore owns durable workspace resources independently of a
// workspace-scoped Store operation.
type WorkspaceControlStore interface {
	ListWorkspaces(context.Context) ([]core.Workspace, error)
	GetWorkspace(context.Context, string) (core.Workspace, error)
	CreateWorkspace(context.Context, string, string, *config.Config) (core.Workspace, error)
}

// Store composes the existing aggregate contracts without changing callers.
// DEC-38; component-persistence; component-verification-strategy.
type Store interface {
	TaskStore
	WorkOrderStore
	DocumentStore
	PlanningStore
	LineageStore
	ActivityStore
	StoreMetadata
}

// TaskStore owns the task, dependency, job and intervention contract.
type TaskStore interface {
	StartOverTaskCommand(context.Context, taskops.TaskLease, core.TaskStartOverRequest) (core.TaskStartOverResult, error)
	CreateTask(ctx context.Context, t core.Task) error
	CreateTaskWithDependencies(ctx context.Context, t core.Task, dependencyIDs []string) error
	CreateTaskWithDependenciesAndContext(ctx context.Context, t core.Task, dependencyIDs []string, attached TaskContextInput) error
	UpdateTaskContext(ctx context.Context, taskID string, change TaskContextChange) (core.TaskContext, error)
	ProposeTaskContext(ctx context.Context, input core.TaskContextProposalInput) (core.TaskContextProposal, bool, error)
	ConfirmTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error)
	DismissTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error)
	ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error)
	// AttachSubmissionGovernance atomically resolves current confirmed scopes
	// and appends only missing design pins before review dispatch (AC-5.1–AC-5.4).
	AttachSubmissionGovernance(ctx context.Context, taskID, repository string, changedPaths []string, attribution SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error)
	ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]CheckpointContextCandidate, error)
	GetTask(ctx context.Context, id string) (core.Task, error)
	GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error)
	ListRepositoryInstallTasks(context.Context, string) ([]core.RepositoryInstallTask, error)
	ListTasks(ctx context.Context) ([]core.Task, error)
	// ListTasksFiltered is ListTasks narrowed by the shared surface predicate,
	// so the stage-grouped board filters through the same store code as the
	// Tasks list instead of a second implementation (AC-2.4). An inactive
	// filter is exactly ListTasks.
	ListTasksFiltered(ctx context.Context, filter TaskFilter) ([]core.Task, error)
	ListTaskPage(ctx context.Context, query TaskOperationsQuery) (TaskPage, error)
	// ListCallerAttentionTaskPage returns only the authenticated caller's
	// assigned, nonterminal tasks that currently satisfy TaskNeedsAttention.
	// Implementations must apply caller and attention predicates before paging.
	ListCallerAttentionTaskPage(ctx context.Context, query CallerAttentionQuery) (TaskPage, error)
	ListTaskOperations(ctx context.Context, query TaskOperationsQuery) (TaskOperationsPage, error)
	ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error)
	// SetTaskHold toggles the per-task worker reservation with an audit
	// event; setting the current value is an idempotent no-op.
	SetTaskHold(ctx context.Context, id string, hold bool) (core.Task, error)
	// AttachTaskBranch remaps Task.Branch under WithTaskSideEffectLock.
	// Same-name is a no-op. A collision names the other open task id.
	AttachTaskBranch(ctx context.Context, taskID, branch string) (core.Task, error)
	SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error)
	RequestChangesCommand(ctx context.Context, lease taskops.TaskLease, request taskops.RequestChanges) (core.Task, error)
	// ChangeTaskPolicyCommand commits only the operator's frozen-policy
	// exception (verify_stage and stage_timeouts.verify). The backend derives
	// the new frozen policy and every order change inside its task
	// transaction; it never selects or reassigns an execution setup
	// (DEC-47, DEC-43, DEC-56; component-task-lifecycle).
	ChangeTaskPolicyCommand(ctx context.Context, lease taskops.TaskLease, request SetupChangeRequest) (SetupChangeResult, error)
	BindTaskApproval(ctx context.Context, id, headSHA string) error
	MarkTaskApprovalStale(ctx context.Context, id, approvedHeadSHA, newHeadSHA, scope, reason string) (bool, error)
	// AdvanceTaskRefreshHead moves a stale approval's refresh target to the
	// head most recently submitted for review, so the next refresh round
	// contracts the pushed fix rather than the head recorded when the
	// approval went stale. Re-advancing to the current refresh
	// head is an idempotent no-op.
	AdvanceTaskRefreshHead(ctx context.Context, id, newHeadSHA string) error
	SkipTaskRefresh(ctx context.Context, id, newHeadSHA, reason string) error
	UpdateTaskClassification(ctx context.Context, id, class string) error
	EnsureTaskEnqueued(ctx context.Context, id string) error
	CreateJob(ctx context.Context, j core.Job) error
	UpdateJob(ctx context.Context, j core.Job) error
	ListJobs(ctx context.Context, taskID string) ([]core.Job, error)
	GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error)
	// WithTaskSideEffectLock serializes one workspace-scoped external side effect across
	// concurrent control-plane instances while its callback rechecks durable
	// state. Implementations must release the lock when the callback returns.
	WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error
	CreateIntervention(ctx context.Context, intervention core.Intervention) error
	// CancelTask atomically records the human intervention, closes the task,
	// and cancels every non-terminal work order.
	CancelTaskCommand(ctx context.Context, lease taskops.TaskLease, intervention core.Intervention) (core.Task, error)
	ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error)
	UpsertTranscript(ctx context.Context, transcript core.Transcript) error
	GetTranscript(ctx context.Context, jobID string) (core.Transcript, error)
	CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error)
	GetSpecVersion(ctx context.Context, taskID string, version int) (core.SpecVersion, bool, error)
	GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error)
	GetApprovedSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error)
	ApproveSpecVersion(ctx context.Context, taskID string, version int) error
	ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error)
	ValidateTaskDependencies(ctx context.Context, dependencyIDs []string) error
	ListBlockingTaskIDs(ctx context.Context, taskID string) ([]string, error)
	ListDependentTaskIDs(ctx context.Context, taskID string) ([]string, error)
	ListDependencyBlockers(ctx context.Context, taskIDs []string) (map[string]DependencyBlockers, error)
	AddTaskDependency(ctx context.Context, request DependencyAdditionRequest) (DependencyAdditionResult, error)
	RemoveTaskDependency(ctx context.Context, request DependencyRemovalRequest) (DependencyRemovalResult, error)
	QueuePullRequestClose(context.Context, core.PullRequestClose) error
	GetPullRequestClose(context.Context, string) (core.PullRequestClose, bool, error)
	UpdatePullRequestClose(context.Context, core.PullRequestClose) error
	QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error
	GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error)
	UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error
	ReconcileGitHubLifecycles(ctx context.Context) (int, error)
	// CreateConflictFixCommand atomically admits one reason-coded conflict-fix
	// attempt and persists its redirect, task transition, job, work order, and
	// dispatch audit. A returned existing order is an idempotent success.
	CreateConflictFixCommand(ctx context.Context, lease taskops.TaskLease, request ConflictFixRequest) (ConflictFixResult, error)
	RequestPlanRevisionCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, rationale string) (PlanRevisionRequestResult, error)
	// CreateClaimedVerificationEvidence atomically derives evidence ownership
	// from an exact live implement claim (component-artifacts; DEC-53).
	CreateClaimedVerificationEvidence(ctx context.Context, request ClaimedVerificationEvidenceRequest, content []byte) (core.Artifact, error)
}

// WorkOrderStore owns the work-order, review-round and worker contract.
type WorkOrderStore interface {
	CreateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder) error
	// CreateStageWorkOrder atomically creates one non-review stage job and work
	// order. It returns false when the same order already exists, making queue
	// redelivery and concurrent dispatch idempotent without a session lock.
	CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error)
	CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, taskID string, jobs []core.Job, orders []core.WorkOrder) error
	RetryReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (ReviewRoundRetryResult, error)
	RecoverInterruptedReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (InterruptedReviewRecoveryResult, error)
	GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error)
	ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error)
	ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error)
	ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error)
	// ListTaskWorkOrdersSnapshot returns persisted order state without applying
	// clock-driven lifecycle transitions. It is for observational responses
	// whose reads must not mutate the work-order lifecycle.
	ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error)
	ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error)
	ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, taskID string, now time.Time) (int, error)
	ClaimWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)
	RedispatchWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, queueTimeout time.Duration) (core.WorkOrder, error)
	PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, request WorkOrderPreemptRequest) (WorkOrderPreemptResult, error)
	RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, queueTimeout time.Duration, refreeze ...*RecoveryRefreeze) (core.WorkOrder, error)
	UpdateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, command ...core.WorkOrderCommand) error
	// SubmitImplementationCommand records an implementation submission and
	// completes the order's own job in one atomic write (component-work-orders).
	SubmitImplementationCommand(ctx context.Context, lease taskops.TaskLease, request ImplementationSubmission) (core.Job, error)
	QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error)
	UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	ReconcileReviewPublications(ctx context.Context) (int, error)
	AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error
	CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error
	ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error)
	CreateWorker(ctx context.Context, worker core.Worker) error
	ListWorkers(ctx context.Context) ([]core.Worker, error)
	ListHarnessModelFailures(ctx context.Context) ([]core.HarnessModelFailure, error)
	AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error)
	HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error)
	RevokeWorker(ctx context.Context, id string) error
	RenewWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, lease time.Duration) (core.WorkOrder, error)
	ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, release core.WorkOrderRelease) (core.WorkOrder, error)
	// CancelPlanRevisionWorkOrderCommand retires the exact released
	// implementation order when the operator approves plan re-entry.
	CancelPlanRevisionWorkOrderCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, attemptID string) (core.WorkOrder, error)
	WorktreeHandoffCommand(context.Context, taskops.TaskLease, string, core.WorkOrderClaimIdentity, string, core.WorktreeHandoffRequest) (core.WorktreeHandoff, error)
	RecordWorkOrderAttemptCheckpoint(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) (bool, error)
	UpsertWorkOrderActivitySnapshot(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, content string) error
	FinalizeWorkOrderAttemptObservability(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) error
	// RecordWorkOrderAttemptCapture records the observational transcript
	// capture of one already-ended attempt for any stage, independently of
	// Git preservation (req-260820-221be8 AC-2.1; DEC-26). It authenticates
	// against the attempt's immutable claim event, derives the reason from
	// the attempt's persisted ending, supersedes only that attempt's activity
	// snapshot, and inserts at most one capture per attempt under the order
	// row lock. It changes no claim, lease, release, or retry state.
	RecordWorkOrderAttemptCapture(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, capture core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error)
	GetWorkOrderActivitySnapshot(ctx context.Context, workOrderID string) (core.WorkOrderActivitySnapshot, bool, error)
	ListWorkOrderTranscriptCaptures(ctx context.Context, workOrderID string) ([]core.WorkOrderTranscriptCapture, error)
	RecordWorkOrderContinuation(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, continuation core.WorkOrderContinuation) (core.WorkOrder, error)
}

// DocumentStore owns the document and decision contract.
type DocumentStore interface {
	// ListDocumentOperatorNotesForTask reads dismissed implementation-origin history in this workspace.
	ListDocumentOperatorNotesForTask(ctx context.Context, taskID string) ([]core.OperatorNote, error)
	// ListRequirementDeliveryEventsForTasks batches only the ordered task
	// context, aggregate review-round, and merge events used by
	// requirement-delivery classification.
	ListRequirementDeliveryEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
	// ListMonitorPullRequestEventsForTasks returns only pull_request.opened events
	// in the current workspace, ordered by time and ID per task. Empty IDs
	// return an empty map without a database read.
	ListMonitorPullRequestEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
	ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, query DocumentEventQuery) (DocumentEventPage, error)
	ListRequirementEvents(ctx context.Context, requirementID string) ([]core.Event, error)
	ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error)
	AcknowledgeRequirementStaleness(ctx context.Context, acknowledgment core.RequirementStalenessAcknowledgment) (core.RequirementStalenessAcknowledgment, error)
	// Requirements are living intent documents: versioned and confirmed, never
	// gated. CreateRequirement commits the document and its
	// first proposed version together; nothing becomes current intent until
	// ConfirmRequirementVersion records an operator's confirmation.
	CreateRequirement(ctx context.Context, requirement core.Requirement, first core.RequirementVersion) (core.Requirement, core.RequirementVersion, error)
	GetRequirement(ctx context.Context, id string) (core.Requirement, error)
	ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error)
	ArchiveRequirement(ctx context.Context, id, actor string, supersededBy []string) error
	RestoreRequirement(ctx context.Context, id, actor string) error
	ListRequirementVersionsByRequirement(ctx context.Context) (map[string][]core.RequirementVersion, error)
	ProposeRequirementVersion(ctx context.Context, version core.RequirementVersion) (core.RequirementVersion, error)
	ConfirmRequirementVersion(ctx context.Context, requirementID string, version int, expectedCurrentVersion ...int) (core.Requirement, core.RequirementVersion, error)
	DismissRequirementVersion(ctx context.Context, requirementID string, version int) (core.Requirement, core.RequirementVersion, error)
	GetRequirementVersion(ctx context.Context, requirementID string, version int) (core.RequirementVersion, error)
	ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error)
	ProposeRequirementServes(ctx context.Context, blueprintTaskID, requirementID string, source core.RequirementServesSource, confirm bool) (core.RequirementServesLink, error)
	ConfirmRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error)
	DismissRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error)
	ListRequirementServes(ctx context.Context) ([]core.RequirementServesLink, error)
	CreateReferenceDocument(ctx context.Context, document core.ReferenceDocument, version core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error)
	SupersedeReferenceDocument(ctx context.Context, documentID string, version core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error)
	GetReferenceDocument(ctx context.Context, documentID string) (core.ReferenceDocument, error)
	ListReferenceDocuments(ctx context.Context, includeDeleted bool) ([]core.ReferenceDocument, error)
	ListReferenceDocumentVersions(ctx context.Context, documentID string) ([]core.ReferenceDocumentVersion, error)
	GetReferenceDocumentVersion(ctx context.Context, documentID string, version int) (core.ReferenceDocumentVersion, error)
	ListReferenceDocumentEvents(ctx context.Context, documentID string) ([]core.Event, error)
	DeleteReferenceDocument(ctx context.Context, documentID string) error
	RecordReferenceDocumentConsulted(ctx context.Context, documentID string, version int, sessionID string) error
	CreateSystemDesign(ctx context.Context, document core.SystemDesign, first core.SystemDesignVersion) (core.SystemDesign, core.SystemDesignVersion, error)
	GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error)
	ListSystemDesigns(ctx context.Context, includeArchived bool) ([]core.SystemDesign, error)
	ArchiveSystemDesign(ctx context.Context, id, actor string, supersededBy []string) error
	RestoreSystemDesign(ctx context.Context, id, actor string) error
	ProposeSystemDesignVersion(ctx context.Context, version core.SystemDesignVersion) (core.SystemDesignVersion, error)
	ConfirmSystemDesignVersion(ctx context.Context, documentID string, version int, expectedCurrentVersion ...int) (core.SystemDesign, core.SystemDesignVersion, error)
	DismissSystemDesignVersion(ctx context.Context, documentID string, version int) (core.SystemDesign, core.SystemDesignVersion, error)
	GetSystemDesignVersion(ctx context.Context, documentID string, version int) (core.SystemDesignVersion, error)
	ListSystemDesignVersions(ctx context.Context, documentID string) ([]core.SystemDesignVersion, error)
	ListSystemDesignEvents(ctx context.Context, documentID string) ([]core.Event, error)
	ListGovernanceDesigns(ctx context.Context, repository string) ([]core.GovernanceDesignContext, error)
	ListPendingSystemDesignVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error)
	// ListClaimBlockingProposalsForTask returns the task's undecided
	// task-authored requirement and System Design versions in the bound
	// workspace, the predicate that withholds its verify and review claims.
	ListClaimBlockingProposalsForTask(ctx context.Context, taskID string) ([]ClaimBlockingProposal, error)
	ListSystemDesignProposalVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error)
	ListSystemDesignProposalEventsForTask(ctx context.Context, taskID string) ([]core.Event, error)
	ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error)
	ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error)
	// ListActiveSystemDesignDriftCounts is the metadata-only projection used by
	// the System Design collection; it must not materialize monitor history.
	ListActiveSystemDesignDriftCounts(ctx context.Context) (map[string]int, error)
	RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error
	ProposeDecision(ctx context.Context, decision core.Decision) (core.Decision, error)
	ConfirmDecision(ctx context.Context, id string) (core.Decision, error)
	DismissDecision(ctx context.Context, id string) (core.Decision, error)
	GetDecision(ctx context.Context, id string) (core.Decision, error)
	ListDecisions(ctx context.Context) ([]core.Decision, error)
	DismissDecisionSupersessionSweep(ctx context.Context, decisionID, documentTier, documentID string) (core.DecisionSupersessionSweepEntry, error)
	// ListPendingProposals normalizes unresolved proposals across the
	// requirement, System Design, and decision tiers (REQ-1, AC-1.2).
	ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error)
	// ListPendingAuthorityProposalsForTask returns only unresolved requirement,
	// System Design, and decision proposals authored by one task. The workspace
	// and task predicates belong to the store boundary so task-run polling never
	// materializes the workspace proposal queue.
	ListPendingAuthorityProposalsForTask(ctx context.Context, taskID string) ([]core.PendingProposal, error)
	// PendingProposalsProjection returns the workspace proposal queue together
	// with the exact task-attention count without materializing the general task,
	// activity, or work-order list projections (REQ-1, REQ-3).
	PendingProposalsProjection(ctx context.Context) (PendingProposalsProjection, error)
}

// PlanningStore owns the planning-session, bundle and artifact contract.
type PlanningStore interface {
	CreatePlanningBundle(ctx context.Context, bundle core.PlanningBundle) (core.PlanningBundle, error)
	GetPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error)
	ListPlanningBundles(ctx context.Context) ([]core.PlanningBundle, error)
	ApprovePlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error)
	RejectPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error)
	// Planning sessions are durable chats that produce at most one artifact and
	// grant no approval authority over it.
	CreatePlanningSession(ctx context.Context, session core.PlanningSession) (core.PlanningSession, error)
	GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error)
	ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error)
	PinPlanningSessionRepo(ctx context.Context, sessionID, repo, revision string) (core.PlanningSession, error)
	RecordPlanningExplorationTokens(ctx context.Context, sessionID string, tokens int) (core.PlanningSession, error)
	AppendPlanningMessage(ctx context.Context, message core.PlanningMessage) (core.PlanningMessage, error)
	ListPlanningMessages(ctx context.Context, sessionID string) ([]core.PlanningMessage, error)
	// WithPlanningSessionRun claims a dedicated per-session run lock without
	// waiting. A competing live run returns ErrPlanningSessionRunConflict before
	// either run can append a user message. Finalize and abandon retain their
	// separate terminal-outcome lock so abandonment can stop model work between
	// planning steps.
	WithPlanningSessionRun(ctx context.Context, sessionID string, fn func(context.Context) error) error
	// WithPlanningSessionFinalization serializes the complete produced-artifact
	// write path against abandonment and invokes fn only while the session is
	// active. Implementations must release the lock when fn returns.
	WithPlanningSessionFinalization(ctx context.Context, sessionID string, fn func(context.Context) error) error
	FinalizePlanningSession(ctx context.Context, request PlanningFinalizeRequest) (core.PlanningSession, error)
	// AbandonPlanningSession closes a session that produced nothing. A
	// finalized session cannot be abandoned; that would strand its lineage.
	AbandonPlanningSession(ctx context.Context, sessionID string, reason ...string) (core.PlanningSession, error)
	CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error)
	CreateTaskWithAttachments(context.Context, core.Task, []string, TaskContextInput, []ArtifactUpload) error
	RepairArtifactMetadata(context.Context, ArtifactRepairRequest) (ArtifactRepairResult, error)
	GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error)
	GetArtifactForPlanningSession(ctx context.Context, id, sessionID string) (core.Artifact, []byte, error)
	ListArtifacts(ctx context.Context) ([]core.Artifact, error)
	ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error)
}

// LineageStore owns the lineage graph contract.
type LineageStore interface {
	ListLineageLinks(ctx context.Context) ([]core.LineageLink, error)
	LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error)
	// ListLineageNeighborhood returns one bounded, workspace-scoped link set
	// for all roots. Callers may derive several per-root traversals from it
	// without repeating a workspace-wide scan.
	ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error)
	// ListRequirementDeliveryLineage returns a bounded, workspace-scoped,
	// directed delivery closure. Only requirement -> task|blueprint `serves`,
	// blueprint -> blueprint_version `versions`, and blueprint_version -> task
	// `materializes` edges are eligible.
	ListRequirementDeliveryLineage(ctx context.Context, requirementID string, budget core.LineageTraversalBudget) ([]core.LineageLink, error)
	ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error)
	// ListLineageNodeRecords resolves label data only for the supplied bounded
	// graph nodes. Implementations must not replace this with workspace-wide
	// entity scans.
	ListLineageNodeRecords(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]LineageNodeRecord, error)
	// ListLineageContextRecords resolves the bounded task outcomes and current
	// confirmed requirement documents used by agent context assembly. It must
	// read only the supplied graph nodes and avoid per-node queries.
	ListLineageContextRecords(ctx context.Context, nodes []core.LineageNode) (LineageContextRecords, error)
	RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error)
}

// ActivityStore owns the event and activity projection contract.
type ActivityStore interface {
	AppendEvent(ctx context.Context, event core.Event) error
	// ListEvents returns the per-task ledger ordered by event time, then ID.
	ListEvents(ctx context.Context, taskID string) ([]core.Event, error)
	// ListEventsAfter treats a nonzero afterID as an anchor event owned by the
	// requested task and workspace and returns the events strictly after its
	// (at, id) tuple in ascending (at, id) order. Zero starts at the beginning.
	// A missing or foreign anchor returns ErrEventAnchorNotFound; readers never
	// compare numeric IDs alone (component-persistence; DEC-39).
	ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error)
	// ReadTaskEventStream returns one bounded (at, id)-ordered page whose
	// events are recorded at or after Since and sort strictly after After.
	// Live streams use overlapping pages to reconcile delayed visibility.
	ReadTaskEventStream(ctx context.Context, query TaskEventStreamQuery) (TaskEventStreamPage, error)
	// ReadTaskEventWindow returns one bounded window of a task-event traversal
	// from a single consistent read view (component-mcp-investigation-reads).
	ReadTaskEventWindow(ctx context.Context, query TaskEventWindowQuery) (TaskEventWindow, error)
	CountEvents(ctx context.Context, taskID, kind string) (int, error)
	// CountEventsSinceHumanIntervention counts task events of the given kind
	// recorded after the latest human intervention on the task — the check-in
	// window since the last human intervention. With no human intervention it counts all events
	// of that kind.
	CountEventsSinceHumanIntervention(ctx context.Context, taskID, kind string) (int, error)
	ListActivityMarkers(ctx context.Context) ([]ActivityMarker, error)
	ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]ActivityMarker, error)
}

// StoreMetadata owns the store metadata contract.
type StoreMetadata interface {
	IsDurable() bool
}

// UnscopedActivityStore is an explicit compatibility capability for the
// in-memory store. Production stores must use the workspace-scoped page path.
type UnscopedActivityStore interface {
	SupportsUnscopedActivity() bool
}

// ActivityMarker contains only the changing fields needed by the activity
// index plus narrow review lifecycle diagnostics. Full job and event histories
// are still loaded only for one selected task.
type ActivityMarker struct {
	TaskID                    string
	LatestStage               core.Stage
	LastEventAt               time.Time
	LastEventID               int64
	ForgeFailure              *ForgeFailure
	ReviewDiagnostics         []ReviewVerdictDiagnostic
	ReviewRecovery            *ReviewRecoveryState
	InterruptedReviewRecovery *InterruptedReviewRecoveryState
	Stalled                   *StalledState
	UserChangesRequested      bool
}

func NewMemory() Store {
	return NewMemoryWithConfig(nil)
}

// NewMemoryWithConfig gives the volatile store the same repository contract as
// the durable store. Blueprint materialization must validate and resolve each
// SUB repository from workspace configuration.
func NewMemoryWithConfig(cfg *config.Config) Store {
	repositories := map[string]map[string]string{}
	if cfg != nil {
		repositories[cfg.Workspace] = map[string]string{}
		for _, repo := range cfg.Repos {
			repositories[cfg.Workspace][repo.Name] = repo.Base
		}
	}
	return &memory{
		tasks:                       map[string]core.Task{},
		repositories:                repositories,
		dependencies:                map[string]map[string]struct{}{},
		jobs:                        map[string][]core.Job{},
		events:                      map[string][]core.Event{},
		lineage:                     map[string]core.LineageLink{},
		interventions:               map[string][]core.Intervention{},
		transcripts:                 map[string]core.Transcript{},
		specs:                       map[string][]core.SpecVersion{},
		workOrders:                  map[string]core.WorkOrder{},
		workOrderActivitySnapshots:  map[string]core.WorkOrderActivitySnapshot{},
		workOrderTranscriptCaptures: map[string][]core.WorkOrderTranscriptCapture{},
		publications:                map[string]core.ReviewPublication{},
		github:                      map[string]core.GitHubLifecycle{},
		requirements:                map[memoryScopedKey]core.Requirement{},
		requirementVersions:         map[memoryScopedKey][]core.RequirementVersion{},
		taskContextProposals:        map[string]core.TaskContextProposal{},
		referenceDocuments:          map[memoryScopedKey]core.ReferenceDocument{},
		referenceDocumentVersions:   map[memoryScopedKey][]core.ReferenceDocumentVersion{},
		systemDesigns:               map[memoryScopedKey]core.SystemDesign{},
		systemDesignVersions:        map[memoryScopedKey][]core.SystemDesignVersion{},
		decisions:                   map[memoryScopedKey]core.Decision{},
		decisionSupersessionSweeps:  map[memoryDecisionSweepKey]core.DecisionSupersessionSweepEntry{},
		planningSessions:            map[memoryScopedKey]core.PlanningSession{},
		planningBundles:             map[memoryScopedKey]core.PlanningBundle{},
		planningMessages:            map[memoryScopedKey][]core.PlanningMessage{},
		artifacts:                   map[memoryArtifactKey]memoryArtifact{},
		pairings:                    map[string]core.WorkerPairing{},
		workers:                     map[string]core.Worker{},
		workspaceMembers:            map[memoryScopedKey]bool{},
		workspaceMemberRoles:        map[memoryScopedKey]core.WorkspaceRole{},
		harnessModelFailures:        map[string]core.HarnessModelFailure{},
		recoveries:                  map[string]struct{}{},
		reviewRetries:               map[string]memoryReviewRoundRetry{},
		interruptedReviewRecoveries: map[string]memoryInterruptedReviewRecovery{},
		setupChanges:                map[string]memorySetupChange{},
		preemptions:                 map[string]memoryWorkOrderPreemption{},
		dependencyAdditions:         map[string]memoryDependencyAddition{},
		dependencyRemovals:          map[string]memoryDependencyRemoval{},
		monitorObservations:         map[string]monitor.ObservationRecord{},
		monitorDrift:                map[string]monitor.Drift{},
		monitorActivity:             map[string][]monitor.Activity{},
	}
}

// memoryScopedKey keys workspace-scoped corpora so the in-memory store honours
// the same isolation the Postgres store enforces with a composite primary key.
type memoryScopedKey struct {
	workspace string
	id        string
}

type memoryDecisionSweepKey struct {
	workspace    string
	decisionID   string
	documentTier string
	documentID   string
}

type memory struct {
	verificationDeliveries      map[string]core.VerificationDelivery
	verificationRows            map[string]VerificationRow
	repositoryInstalls          map[memoryScopedKey][]core.RepositoryInstallTask
	mu                          sync.RWMutex
	tasks                       map[string]core.Task
	repositories                map[string]map[string]string
	dependencies                map[string]map[string]struct{}
	jobs                        map[string][]core.Job
	events                      map[string][]core.Event
	lineage                     map[string]core.LineageLink
	interventions               map[string][]core.Intervention
	transcripts                 map[string]core.Transcript
	specs                       map[string][]core.SpecVersion
	workOrders                  map[string]core.WorkOrder
	workOrderActivitySnapshots  map[string]core.WorkOrderActivitySnapshot
	workOrderTranscriptCaptures map[string][]core.WorkOrderTranscriptCapture
	publications                map[string]core.ReviewPublication
	pullRequestCloses           map[string]core.PullRequestClose
	github                      map[string]core.GitHubLifecycle
	requirements                map[memoryScopedKey]core.Requirement
	requirementVersions         map[memoryScopedKey][]core.RequirementVersion
	taskContextProposals        map[string]core.TaskContextProposal
	referenceDocuments          map[memoryScopedKey]core.ReferenceDocument
	referenceDocumentVersions   map[memoryScopedKey][]core.ReferenceDocumentVersion
	systemDesigns               map[memoryScopedKey]core.SystemDesign
	systemDesignVersions        map[memoryScopedKey][]core.SystemDesignVersion
	decisions                   map[memoryScopedKey]core.Decision
	decisionSupersessionSweeps  map[memoryDecisionSweepKey]core.DecisionSupersessionSweepEntry
	planningSessions            map[memoryScopedKey]core.PlanningSession
	planningBundles             map[memoryScopedKey]core.PlanningBundle
	planningMessages            map[memoryScopedKey][]core.PlanningMessage
	artifacts                   map[memoryArtifactKey]memoryArtifact
	artifactRepairs             map[memoryScopedKey]ArtifactRepairReceipt
	pairings                    map[string]core.WorkerPairing
	workers                     map[string]core.Worker
	workspaceMembers            map[memoryScopedKey]bool
	workspaceMemberRoles        map[memoryScopedKey]core.WorkspaceRole
	harnessModelFailures        map[string]core.HarnessModelFailure
	recoveries                  map[string]struct{}
	reviewRetries               map[string]memoryReviewRoundRetry
	interruptedReviewRecoveries map[string]memoryInterruptedReviewRecovery
	setupChanges                map[string]memorySetupChange
	preemptions                 map[string]memoryWorkOrderPreemption
	dependencyAdditions         map[string]memoryDependencyAddition
	dependencyRemovals          map[string]memoryDependencyRemoval
	monitorObservations         map[string]monitor.ObservationRecord
	monitorDrift                map[string]monitor.Drift
	monitorLastSuccess          map[string]time.Time
	monitorError                map[string]string
	monitorErrorCategory        map[string]string
	monitorBackoff              map[string]time.Time
	monitorActivity             map[string][]monitor.Activity
	nextMonitorActivityID       int64
	nextEventID                 int64
	nextReviewID                int64
	taskLocks                   sync.Map
}

func (m *memory) SupportsUnscopedActivity() bool { return true }

func (m *memory) IsDurable() bool { return false }

func workspaceOrDefault(ctx context.Context, fallback string) string {
	if workspace, ok := WorkspaceFromContext(ctx); ok && workspace != "" {
		return workspace
	}
	return fallback
}

func (m *memory) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	workspace, _ := WorkspaceFromContext(ctx)
	value, _ := m.taskLocks.LoadOrStore("task/"+workspace+"/"+taskID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return fn(ctx)
}

func (m *memory) AppendEvent(ctx context.Context, event core.Event) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[event.TaskID]; !ok {
		return fmt.Errorf("task %s not found", event.TaskID)
	}
	if event.JobID != "" {
		job, _, ok := m.findJobLocked(event.JobID)
		if !ok || job.TaskID != event.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", event.JobID, event.TaskID)
		}
	}
	m.appendEventLocked(ctx, event)
	return nil
}

func (m *memory) ListEvents(_ context.Context, taskID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := append([]core.Event(nil), m.events[taskID]...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events, nil
}

func (m *memory) CountEvents(_ context.Context, taskID, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, event := range m.events[taskID] {
		if event.Kind == kind {
			count++
		}
	}
	return count, nil
}

func (m *memory) CountEventsSinceHumanIntervention(_ context.Context, taskID, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countEventsSinceHumanInterventionLocked(taskID, kind), nil
}

func (m *memory) countEventsSinceHumanInterventionLocked(taskID, kind string) int {
	var since time.Time
	for _, intervention := range m.interventions[taskID] {
		if intervention.ActorRole == core.ActorHuman && intervention.At.After(since) {
			since = intervention.At
		}
	}
	count := 0
	for _, event := range m.events[taskID] {
		if event.Kind == kind && event.At.After(since) {
			count++
		}
	}
	return count
}

func (m *memory) ListActivityMarkers(ctx context.Context) ([]ActivityMarker, error) {
	orders, err := m.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	var implementTaskIDs []string
	seenImplementTask := map[string]bool{}
	for _, order := range orders {
		if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !seenImplementTask[order.TaskID] {
			implementTaskIDs = append(implementTaskIDs, order.TaskID)
			seenImplementTask[order.TaskID] = true
		}
	}
	blockersByTask, err := m.ListDependencyBlockers(ctx, implementTaskIDs)
	if err != nil {
		return nil, err
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	for _, order := range orders {
		if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
			blockers := blockersByTask[order.TaskID]
			order.BlockingTaskIDs = append([]string(nil), blockers.BlockingTaskIDs...)
			order.UnsatisfiableTaskIDs = append([]string(nil), blockers.UnsatisfiableTaskIDs...)
			if len(order.BlockingTaskIDs) > 0 {
				order.Claimable = false
			}
		}
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	markers := make([]ActivityMarker, 0, len(m.tasks))
	for id, task := range m.tasks {
		marker := ActivityMarker{TaskID: id, LastEventAt: task.CreatedAt}
		for _, order := range ordersByTask[id] {
			if order.State == core.WorkOrderClaimed {
				marker.LatestStage = order.Stage
			}
		}
		if jobs := append([]core.Job(nil), m.jobs[id]...); marker.LatestStage == "" && len(jobs) != 0 {
			sortJobs(jobs)
			marker.LatestStage = jobs[len(jobs)-1].Stage
		}
		if latest, ok := LatestTaskEvent(m.events[id], ""); ok {
			marker.LastEventAt, marker.LastEventID = latest.At, latest.ID
		}
		marker.ForgeFailure = LatestForgeFailure(m.events[id])
		marker.ReviewDiagnostics = ReviewVerdictDiagnostics(ordersByTask[id], m.events[id], time.Now().UTC())
		marker.ReviewRecovery = ReviewRecoveryNeeded(ordersByTask[id], m.events[id])
		marker.InterruptedReviewRecovery = InterruptedReviewRecoveryNeeded(task, CurrentReviewOrders(ordersByTask[id], m.events[id]), m.events[id])
		marker.Stalled = StalledTask(ordersByTask[id])
		marker.UserChangesRequested = UserRequestChangesPending(m.events[id])
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].TaskID < markers[j].TaskID })
	return markers, nil
}

func (m *memory) ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]ActivityMarker, error) {
	markers, err := m.ListActivityMarkers(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(taskIDs))
	for _, taskID := range taskIDs {
		wanted[taskID] = true
	}
	result := make([]ActivityMarker, 0, len(taskIDs))
	for _, marker := range markers {
		if wanted[marker.TaskID] {
			result = append(result, marker)
		}
	}
	return result, nil
}

func (m *memory) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, _, ok := m.findJobLocked(transcript.JobID)
	if !ok {
		return fmt.Errorf("job %s not found", transcript.JobID)
	}
	if transcript.CreatedAt.IsZero() {
		transcript.CreatedAt = time.Now().UTC()
	}
	if artifactID := strings.TrimPrefix(transcript.URI, "artifact://"); artifactID != transcript.URI {
		task := m.tasks[job.TaskID]
		key := memoryArtifactKey{workspace: task.Workspace, id: artifactID}
		if artifact, exists := m.artifacts[key]; exists {
			hasExplicitAudit := false
			for _, link := range artifact.links {
				hasExplicitAudit = hasExplicitAudit || link.TaskID == job.TaskID && link.Role == core.ArtifactRoleGeneratedAudit
			}
			for i := range artifact.links {
				if !hasExplicitAudit && artifact.links[i].TaskID == job.TaskID && artifact.links[i].Role.ModelInputEligible() {
					artifact.links[i].Role = core.ArtifactRoleGeneratedAudit
				}
			}
			m.artifacts[key] = artifact
		}
	}
	m.transcripts[transcript.JobID] = transcript
	m.appendEventLocked(ctx, core.Event{
		TaskID: job.TaskID, JobID: job.ID, Kind: "transcript.persisted",
		Payload: core.JSONPayload(map[string]any{"uri": transcript.URI, "redaction_stats": transcript.RedactionStats}),
	})
	return nil
}

func (m *memory) GetTranscript(_ context.Context, jobID string) (core.Transcript, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	transcript, ok := m.transcripts[jobID]
	if !ok {
		return core.Transcript{}, fmt.Errorf("transcript for job %s not found", jobID)
	}
	return transcript, nil
}

// appendEventLocked records one event under the held mutex. Every mutating
// entry point checks RequireActor before it changes any state, so a missing
// actor here is a programming error, never a silent default
// (component-persistence, Actor context).
func (m *memory) appendEventLocked(ctx context.Context, event core.Event) {
	actor, err := RequireActor(ctx)
	if err != nil {
		panic(fmt.Errorf("volatile event %q reached the ledger without an actor check: %w", event.Kind, err))
	}
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = core.JSONPayload(struct{}{})
	}
	m.nextEventID++
	event.ID = m.nextEventID
	m.events[event.TaskID] = append(m.events[event.TaskID], event)
	workspace := workspaceOrDefault(ctx, "")
	if event.TaskID != "" {
		if task, ok := m.tasks[event.TaskID]; ok {
			workspace = task.Workspace
		}
	}
	projection := projectLineageEvent(workspace, event)
	for _, link := range projection.Suppresses {
		delete(m.lineage, lineageLinkKey(link))
	}
	for _, link := range projection.Links {
		key := lineageLinkKey(link)
		if _, exists := m.lineage[key]; !exists {
			m.lineage[key] = link
		}
	}
}
