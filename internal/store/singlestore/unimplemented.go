// Explicit unimplemented backend methods. Aggregate tasks replace their own lines.
package singlestore

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) AbandonPlanningSession(ctx context.Context, sessionID string, reason ...string) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	return store.ErrNotImplemented
}
func (s *Store) AcknowledgeRequirementStaleness(ctx context.Context, acknowledgment core.RequirementStalenessAcknowledgment) (core.RequirementStalenessAcknowledgment, error) {
	return zero[core.RequirementStalenessAcknowledgment](), store.ErrNotImplemented
}
func (s *Store) AddTaskDependency(ctx context.Context, request store.DependencyAdditionRequest) (store.DependencyAdditionResult, error) {
	return zero[store.DependencyAdditionResult](), store.ErrNotImplemented
}
func (s *Store) AdvanceTaskRefreshHead(ctx context.Context, id, newHeadSHA string) error {
	return store.ErrNotImplemented
}
func (s *Store) AppendEvent(ctx context.Context, event core.Event) error {
	return store.ErrNotImplemented
}
func (s *Store) AppendPlanningMessage(ctx context.Context, message core.PlanningMessage) (core.PlanningMessage, error) {
	return zero[core.PlanningMessage](), store.ErrNotImplemented
}
func (s *Store) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, taskID string, now time.Time) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) ApprovePlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	return zero[core.PlanningBundle](), store.ErrNotImplemented
}
func (s *Store) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	return store.ErrNotImplemented
}
func (s *Store) ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error) {
	return zero[[]core.Task](), store.ErrNotImplemented
}
func (s *Store) ArchiveRequirement(ctx context.Context, id, actor string, supersededBy []string) error {
	return store.ErrNotImplemented
}
func (s *Store) ArchiveSystemDesign(ctx context.Context, id, actor string, supersededBy []string) error {
	return store.ErrNotImplemented
}
func (s *Store) AssignTaskFeature(ctx context.Context, taskID, featureID string) error {
	return store.ErrNotImplemented
}
func (s *Store) AttachSubmissionGovernance(ctx context.Context, taskID, repository string, changedPaths []string, attribution store.SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	return zero[[]core.TaskDesignContext](), store.ErrNotImplemented
}
func (s *Store) AuditMonitor(context.Context, string, map[string]any) error {
	return store.ErrNotImplemented
}
func (s *Store) AuditTask(context.Context, string, string, map[string]any) error {
	return store.ErrNotImplemented
}
func (s *Store) AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error) {
	return zero[core.Worker](), store.ErrNotImplemented
}
func (s *Store) AuthorizeDeployment(context.Context, string, core.Capability) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) AuthorizeWorkspace(context.Context, string, string, core.Capability) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) BindTaskApproval(ctx context.Context, id, headSHA string) error {
	return store.ErrNotImplemented
}
func (s *Store) BootstrapIdentity(context.Context, config.FirstOperatorIdentity, string) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) CancelPlanRevisionWorkOrderCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, attemptID string) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) CancelTaskCommand(ctx context.Context, lease taskops.TaskLease, intervention core.Intervention) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) ChangeTaskSetupCommand(ctx context.Context, lease taskops.TaskLease, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	return zero[store.SetupChangeResult](), store.ErrNotImplemented
}
func (s *Store) ClaimWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) ConfigureForgeTokenEncryptionKey([]byte) {}
func (s *Store) ConfirmDecision(ctx context.Context, id string) (core.Decision, error) {
	return zero[core.Decision](), store.ErrNotImplemented
}
func (s *Store) ConfirmRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	return zero[core.RequirementServesLink](), store.ErrNotImplemented
}
func (s *Store) ConfirmRequirementVersion(ctx context.Context, requirementID string, version int, expectedCurrentVersion ...int) (core.Requirement, core.RequirementVersion, error) {
	return zero[core.Requirement](), zero[core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) ConfirmSystemDesignVersion(ctx context.Context, documentID string, version int, expectedCurrentVersion ...int) (core.SystemDesign, core.SystemDesignVersion, error) {
	return zero[core.SystemDesign](), zero[core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) ConfirmTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return zero[core.TaskContextProposal](), store.ErrNotImplemented
}
func (s *Store) ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error) {
	return zero[core.WorkerPairing](), store.ErrNotImplemented
}
func (s *Store) CountEvents(ctx context.Context, taskID, kind string) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) CountEventsSinceHumanIntervention(ctx context.Context, taskID, kind string) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	return zero[core.Artifact](), store.ErrNotImplemented
}
func (s *Store) CreateClaimedVerificationEvidence(ctx context.Context, request store.ClaimedVerificationEvidenceRequest, content []byte) (core.Artifact, error) {
	return zero[core.Artifact](), store.ErrNotImplemented
}
func (s *Store) CreateConflictFixCommand(ctx context.Context, lease taskops.TaskLease, request store.ConflictFixRequest) (store.ConflictFixResult, error) {
	return zero[store.ConflictFixResult](), store.ErrNotImplemented
}
func (s *Store) CreateFeature(ctx context.Context, feature core.Feature) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateJob(ctx context.Context, j core.Job) error { return store.ErrNotImplemented }
func (s *Store) CreatePlanningBundle(ctx context.Context, bundle core.PlanningBundle) (core.PlanningBundle, error) {
	return zero[core.PlanningBundle](), store.ErrNotImplemented
}
func (s *Store) CreatePlanningSession(ctx context.Context, session core.PlanningSession) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) CreateReferenceDocument(ctx context.Context, document core.ReferenceDocument, version core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	return zero[core.ReferenceDocument](), zero[core.ReferenceDocumentVersion](), store.ErrNotImplemented
}
func (s *Store) CreateRequirement(ctx context.Context, requirement core.Requirement, first core.RequirementVersion) (core.Requirement, core.RequirementVersion, error) {
	return zero[core.Requirement](), zero[core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	return zero[core.SpecVersion](), store.ErrNotImplemented
}
func (s *Store) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) CreateSystemDesign(ctx context.Context, document core.SystemDesign, first core.SystemDesignVersion) (core.SystemDesign, core.SystemDesignVersion, error) {
	return zero[core.SystemDesign](), zero[core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) CreateTask(ctx context.Context, t core.Task) error { return store.ErrNotImplemented }
func (s *Store) CreateTaskWithDependencies(ctx context.Context, t core.Task, dependencyIDs []string) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateTaskWithDependenciesAndContext(ctx context.Context, t core.Task, dependencyIDs []string, attached store.TaskContextInput) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateWorker(ctx context.Context, worker core.Worker) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error {
	return store.ErrNotImplemented
}
func (s *Store) DeleteForgeToken(context.Context, string) error { return store.ErrNotImplemented }
func (s *Store) DeleteReferenceDocument(ctx context.Context, documentID string) error {
	return store.ErrNotImplemented
}
func (s *Store) DeleteWorkspaceForgeToken(context.Context, string) error {
	return store.ErrNotImplemented
}
func (s *Store) DismissDecision(ctx context.Context, id string) (core.Decision, error) {
	return zero[core.Decision](), store.ErrNotImplemented
}
func (s *Store) DismissDecisionSupersessionSweep(ctx context.Context, decisionID, documentTier, documentID string) (core.DecisionSupersessionSweepEntry, error) {
	return zero[core.DecisionSupersessionSweepEntry](), store.ErrNotImplemented
}
func (s *Store) DismissRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	return zero[core.RequirementServesLink](), store.ErrNotImplemented
}
func (s *Store) DismissRequirementVersion(ctx context.Context, requirementID string, version int) (core.Requirement, core.RequirementVersion, error) {
	return zero[core.Requirement](), zero[core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) DismissSystemDesignVersion(ctx context.Context, documentID string, version int) (core.SystemDesign, core.SystemDesignVersion, error) {
	return zero[core.SystemDesign](), zero[core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) DismissTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return zero[core.TaskContextProposal](), store.ErrNotImplemented
}
func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return store.ErrNotImplemented
}
func (s *Store) FinalizePlanningSession(ctx context.Context, request store.PlanningFinalizeRequest) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) FinalizeWorkOrderAttemptObservability(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) error {
	return store.ErrNotImplemented
}
func (s *Store) FindOpenMonitorTask(context.Context, string, monitor.SignalKind) (string, bool, error) {
	return zero[string](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetApprovedSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	return zero[core.SpecVersion](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	return zero[core.Artifact](), zero[[]byte](), store.ErrNotImplemented
}
func (s *Store) GetArtifactForPlanningSession(ctx context.Context, id, sessionID string) (core.Artifact, []byte, error) {
	return zero[core.Artifact](), zero[[]byte](), store.ErrNotImplemented
}
func (s *Store) GetCallerIdentity(context.Context, string, string) (core.CallerIdentity, error) {
	return zero[core.CallerIdentity](), store.ErrNotImplemented
}
func (s *Store) GetDecision(ctx context.Context, id string) (core.Decision, error) {
	return zero[core.Decision](), store.ErrNotImplemented
}
func (s *Store) GetForgeTokenForUse(context.Context, string) (core.ForgeTokenCredential, error) {
	return zero[core.ForgeTokenCredential](), store.ErrNotImplemented
}
func (s *Store) GetForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error) {
	return zero[core.ForgeTokenStatus](), store.ErrNotImplemented
}
func (s *Store) GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	return zero[core.GitHubLifecycle](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	return zero[core.SpecVersion](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	return zero[core.PlanningBundle](), store.ErrNotImplemented
}
func (s *Store) GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	return zero[core.Requirement](), store.ErrNotImplemented
}
func (s *Store) GetRequirementVersion(ctx context.Context, requirementID string, version int) (core.RequirementVersion, error) {
	return zero[core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error) {
	return zero[core.ReviewPublication](), store.ErrNotImplemented
}
func (s *Store) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	return zero[core.SystemDesign](), store.ErrNotImplemented
}
func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) GetWorkOrderActivitySnapshot(ctx context.Context, workOrderID string) (core.WorkOrderActivitySnapshot, bool, error) {
	return zero[core.WorkOrderActivitySnapshot](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetWorkspaceForgeTokenForUse(context.Context, string) (core.WorkspaceForgeTokenCredential, error) {
	return zero[core.WorkspaceForgeTokenCredential](), store.ErrNotImplemented
}
func (s *Store) GetWorkspaceForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error) {
	return zero[core.ForgeTokenStatus](), store.ErrNotImplemented
}
func (s *Store) GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error) {
	return zero[core.MembershipGrant](), store.ErrNotImplemented
}
func (s *Store) HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error) {
	return zero[core.Worker](), store.ErrNotImplemented
}
func (s *Store) IssueAgentCredential(context.Context, string, string) (store.IssuedAgentCredential, error) {
	return zero[store.IssuedAgentCredential](), store.ErrNotImplemented
}
func (s *Store) IssueOwnPersonalAccessToken(context.Context, string, string) (core.IssuedPersonalAccessToken, error) {
	return zero[core.IssuedPersonalAccessToken](), store.ErrNotImplemented
}
func (s *Store) IssueSignInLink(context.Context, string) (core.IssuedSignInLink, error) {
	return zero[core.IssuedSignInLink](), store.ErrNotImplemented
}
func (s *Store) LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) LinkTask(context.Context, string, string, string) (monitor.ObservationRecord, error) {
	return zero[monitor.ObservationRecord](), store.ErrNotImplemented
}
func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	return zero[[]core.Artifact](), store.ErrNotImplemented
}
func (s *Store) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	return zero[[]core.Artifact](), store.ErrNotImplemented
}
func (s *Store) ListBlockingTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	return zero[[]string](), store.ErrNotImplemented
}
func (s *Store) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	return zero[[]core.Decision](), store.ErrNotImplemented
}
func (s *Store) ListDependencyBlockers(ctx context.Context, taskIDs []string) (map[string]store.DependencyBlockers, error) {
	return zero[map[string]store.DependencyBlockers](), store.ErrNotImplemented
}
func (s *Store) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, query store.DocumentEventQuery) (store.DocumentEventPage, error) {
	return zero[store.DocumentEventPage](), store.ErrNotImplemented
}
func (s *Store) ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error) {
	return zero[[]string](), store.ErrNotImplemented
}
func (s *Store) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListForgeTokensForRedaction(context.Context) ([]string, error) {
	return zero[[]string](), store.ErrNotImplemented
}
func (s *Store) ListHarnessModelFailures(ctx context.Context) ([]core.HarnessModelFailure, error) {
	return zero[[]core.HarnessModelFailure](), store.ErrNotImplemented
}
func (s *Store) ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error) {
	return zero[[]core.Intervention](), store.ErrNotImplemented
}
func (s *Store) ListLineageLinks(ctx context.Context) ([]core.LineageLink, error) {
	return zero[[]core.LineageLink](), store.ErrNotImplemented
}
func (s *Store) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	return zero[[]core.LineageLink](), store.ErrNotImplemented
}
func (s *Store) ListOwnPersonalAccessTokens(context.Context, string) ([]core.PersonalAccessToken, error) {
	return zero[[]core.PersonalAccessToken](), store.ErrNotImplemented
}
func (s *Store) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	return zero[[]core.PendingProposal](), store.ErrNotImplemented
}
func (s *Store) ListPendingSystemDesignVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error) {
	return zero[[]core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) ListPlanningMessages(ctx context.Context, sessionID string) ([]core.PlanningMessage, error) {
	return zero[[]core.PlanningMessage](), store.ErrNotImplemented
}
func (s *Store) ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error) {
	return zero[[]core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) ListReferenceDocumentEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListReferenceDocumentVersions(ctx context.Context, documentID string) ([]core.ReferenceDocumentVersion, error) {
	return zero[[]core.ReferenceDocumentVersion](), store.ErrNotImplemented
}
func (s *Store) ListReferenceDocuments(ctx context.Context, includeDeleted bool) ([]core.ReferenceDocument, error) {
	return zero[[]core.ReferenceDocument](), store.ErrNotImplemented
}
func (s *Store) ListRequirementDeliveryLineage(ctx context.Context, requirementID string, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	return zero[[]core.LineageLink](), store.ErrNotImplemented
}
func (s *Store) ListRequirementEvents(ctx context.Context, requirementID string) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListRequirementServes(ctx context.Context) ([]core.RequirementServesLink, error) {
	return zero[[]core.RequirementServesLink](), store.ErrNotImplemented
}
func (s *Store) ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error) {
	return zero[[]core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error) {
	return zero[[]core.Requirement](), store.ErrNotImplemented
}
func (s *Store) ListSystemDesignEvents(ctx context.Context, documentID string) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListSystemDesignProposalEventsForTask(ctx context.Context, taskID string) ([]core.Event, error) {
	return zero[[]core.Event](), store.ErrNotImplemented
}
func (s *Store) ListSystemDesignProposalVersionsForTask(ctx context.Context, taskID string) ([]core.SystemDesignVersion, error) {
	return zero[[]core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) ListSystemDesignVersions(ctx context.Context, documentID string) ([]core.SystemDesignVersion, error) {
	return zero[[]core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) ListSystemDesigns(context.Context, bool) ([]core.SystemDesign, error) {
	return zero[[]core.SystemDesign](), store.ErrNotImplemented
}
func (s *Store) ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	return zero[[]core.TaskContextProposal](), store.ErrNotImplemented
}
func (s *Store) ListTaskOperations(ctx context.Context, query store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	return zero[store.TaskOperationsPage](), store.ErrNotImplemented
}
func (s *Store) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	return zero[[]core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	return zero[[]core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	return zero[[]core.Task](), store.ErrNotImplemented
}
func (s *Store) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	return zero[[]core.Task](), store.ErrNotImplemented
}
func (s *Store) ListWorkOrderTranscriptCaptures(ctx context.Context, workOrderID string) ([]core.WorkOrderTranscriptCapture, error) {
	return zero[[]core.WorkOrderTranscriptCapture](), store.ErrNotImplemented
}
func (s *Store) ListWorkers(ctx context.Context) ([]core.Worker, error) {
	return zero[[]core.Worker](), store.ErrNotImplemented
}
func (s *Store) ListWorkspaceInvitations(context.Context, string) ([]core.WorkspaceInvitation, error) {
	return zero[[]core.WorkspaceInvitation](), store.ErrNotImplemented
}
func (s *Store) ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error) {
	return zero[[]core.WorkspaceMembership](), store.ErrNotImplemented
}
func (s *Store) ListWorkspacesForUser(context.Context, string) ([]core.Workspace, error) {
	return zero[[]core.Workspace](), store.ErrNotImplemented
}
func (s *Store) MarkTaskApprovalStale(ctx context.Context, id, approvedHeadSHA, newHeadSHA, scope, reason string) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) MonitorStatus(context.Context, bool, time.Time) (monitor.Status, error) {
	return zero[monitor.Status](), store.ErrNotImplemented
}
func (s *Store) Observe(context.Context, monitor.Observation) (monitor.ObservationRecord, bool, error) {
	return zero[monitor.ObservationRecord](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) PendingProposalsProjection(ctx context.Context) (store.PendingProposalsProjection, error) {
	return zero[store.PendingProposalsProjection](), store.ErrNotImplemented
}
func (s *Store) PinPlanningSessionRepo(ctx context.Context, sessionID, repo, revision string) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.WorkOrderPreemptRequest) (store.WorkOrderPreemptResult, error) {
	return zero[store.WorkOrderPreemptResult](), store.ErrNotImplemented
}
func (s *Store) ProposeDecision(ctx context.Context, decision core.Decision) (core.Decision, error) {
	return zero[core.Decision](), store.ErrNotImplemented
}
func (s *Store) ProposeRequirementServes(ctx context.Context, blueprintTaskID, requirementID string, source core.RequirementServesSource, confirm bool) (core.RequirementServesLink, error) {
	return zero[core.RequirementServesLink](), store.ErrNotImplemented
}
func (s *Store) ProposeRequirementVersion(ctx context.Context, version core.RequirementVersion) (core.RequirementVersion, error) {
	return zero[core.RequirementVersion](), store.ErrNotImplemented
}
func (s *Store) ProposeSystemDesignVersion(ctx context.Context, version core.SystemDesignVersion) (core.SystemDesignVersion, error) {
	return zero[core.SystemDesignVersion](), store.ErrNotImplemented
}
func (s *Store) ProposeTaskContext(ctx context.Context, input core.TaskContextProposalInput) (core.TaskContextProposal, bool, error) {
	return zero[core.TaskContextProposal](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) ProvisionIdentityUser(context.Context, string, string) (core.IdentityUser, error) {
	return zero[core.IdentityUser](), store.ErrNotImplemented
}
func (s *Store) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return store.ErrNotImplemented
}
func (s *Store) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return store.ErrNotImplemented
}
func (s *Store) RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	return zero[core.LineageRebuildResult](), store.ErrNotImplemented
}
func (s *Store) ReconcileReviewPublications(ctx context.Context) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) RecordDrift(context.Context, monitor.Drift) (monitor.Drift, bool, error) {
	return zero[monitor.Drift](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) RecordInvitationDelivery(context.Context, string, string) error {
	return store.ErrNotImplemented
}
func (s *Store) RecordMonitorFailure(context.Context, string, string, time.Time) error {
	return store.ErrNotImplemented
}
func (s *Store) RecordMonitorSuccess(context.Context, time.Time) error {
	return store.ErrNotImplemented
}
func (s *Store) RecordPlanningExplorationTokens(ctx context.Context, sessionID string, tokens int) (core.PlanningSession, error) {
	return zero[core.PlanningSession](), store.ErrNotImplemented
}
func (s *Store) RecordReferenceDocumentConsulted(ctx context.Context, documentID string, version int, sessionID string) error {
	return store.ErrNotImplemented
}
func (s *Store) RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error {
	return store.ErrNotImplemented
}
func (s *Store) RecordWorkOrderAttemptCheckpoint(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
}
func (s *Store) RecordWorkOrderContinuation(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, continuation core.WorkOrderContinuation) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RecoverInterruptedReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request store.InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	return zero[store.InterruptedReviewRecoveryResult](), store.ErrNotImplemented
}
func (s *Store) RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, queueTimeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RedeemSignInLink(context.Context, string) (core.DashboardSession, core.IdentityUser, error) {
	return zero[core.DashboardSession](), zero[core.IdentityUser](), store.ErrNotImplemented
}
func (s *Store) RedispatchWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, queueTimeout time.Duration) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RefreshWorkOrderHarnessSnapshot(ctx context.Context, id string, snapshot *core.HarnessSnapshot) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RejectPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	return zero[core.PlanningBundle](), store.ErrNotImplemented
}
func (s *Store) ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, release core.WorkOrderRelease) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RemoveTaskDependency(ctx context.Context, request store.DependencyRemovalRequest) (store.DependencyRemovalResult, error) {
	return zero[store.DependencyRemovalResult](), store.ErrNotImplemented
}
func (s *Store) RenewWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, lease time.Duration) (core.WorkOrder, error) {
	return zero[core.WorkOrder](), store.ErrNotImplemented
}
func (s *Store) RequestChangesCommand(ctx context.Context, lease taskops.TaskLease, request taskops.RequestChanges) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) RequestPlanRevisionCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, rationale string) (store.PlanRevisionRequestResult, error) {
	return zero[store.PlanRevisionRequestResult](), store.ErrNotImplemented
}
func (s *Store) ResolveCausalSystemDesignMerge(context.Context, string, string, string, int64, string, []string, bool) (monitor.SystemDesignMergeJudgment, error) {
	return zero[monitor.SystemDesignMergeJudgment](), store.ErrNotImplemented
}
func (s *Store) ResolveDrift(context.Context, string, string, string) (monitor.Drift, error) {
	return zero[monitor.Drift](), store.ErrNotImplemented
}
func (s *Store) RestoreRequirement(ctx context.Context, id, actor string) error {
	return store.ErrNotImplemented
}
func (s *Store) RestoreSystemDesign(ctx context.Context, id, actor string) error {
	return store.ErrNotImplemented
}
func (s *Store) RetryReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request store.ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	return zero[store.ReviewRoundRetryResult](), store.ErrNotImplemented
}
func (s *Store) RevokeDashboardSession(context.Context, string, string) error {
	return store.ErrNotImplemented
}
func (s *Store) RevokeOwnPersonalAccessToken(context.Context, string, string) (core.PersonalAccessToken, error) {
	return zero[core.PersonalAccessToken](), store.ErrNotImplemented
}
func (s *Store) RevokeRunAgentCredential(context.Context, string, string, store.RunAgentCredentialBinding) error {
	return store.ErrNotImplemented
}
func (s *Store) RevokeWorker(ctx context.Context, id string) error { return store.ErrNotImplemented }
func (s *Store) RevokeWorkspaceInvitation(context.Context, string, string) error {
	return store.ErrNotImplemented
}
func (s *Store) RevokeWorkspaceRole(context.Context, string, string) error {
	return store.ErrNotImplemented
}
func (s *Store) SetOwnDisplayName(context.Context, string, string, string) (core.CallerIdentity, error) {
	return zero[core.CallerIdentity](), store.ErrNotImplemented
}
func (s *Store) SetOwnPassword(context.Context, string, string, string, string) error {
	return store.ErrNotImplemented
}
func (s *Store) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) SetTaskHold(ctx context.Context, id string, hold bool) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) SignInWithPassword(context.Context, string, string) (core.DashboardSession, core.IdentityUser, error) {
	return zero[core.DashboardSession](), zero[core.IdentityUser](), store.ErrNotImplemented
}
func (s *Store) SkipTaskRefresh(ctx context.Context, id, newHeadSHA, reason string) error {
	return store.ErrNotImplemented
}
func (s *Store) StoreForgeToken(context.Context, string, string, string) (core.ForgeTokenStatus, error) {
	return zero[core.ForgeTokenStatus](), store.ErrNotImplemented
}
func (s *Store) StoreWorkspaceForgeToken(context.Context, string, string, string) (core.ForgeTokenStatus, error) {
	return zero[core.ForgeTokenStatus](), store.ErrNotImplemented
}
func (s *Store) SupersedeReferenceDocument(ctx context.Context, documentID string, version core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error) {
	return zero[core.ReferenceDocumentVersion](), store.ErrNotImplemented
}
func (s *Store) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return store.ErrNotImplemented
}
func (s *Store) UpdateJob(ctx context.Context, j core.Job) error { return store.ErrNotImplemented }
func (s *Store) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return store.ErrNotImplemented
}
func (s *Store) UpdateTaskClassification(ctx context.Context, id, class string) error {
	return store.ErrNotImplemented
}
func (s *Store) UpdateTaskContext(ctx context.Context, taskID string, change store.TaskContextChange) (core.TaskContext, error) {
	return zero[core.TaskContext](), store.ErrNotImplemented
}
func (s *Store) UpdateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, command ...core.WorkOrderCommand) error {
	return store.ErrNotImplemented
}
func (s *Store) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	return store.ErrNotImplemented
}
func (s *Store) UpsertWorkOrderActivitySnapshot(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, content string) error {
	return store.ErrNotImplemented
}
func (s *Store) ValidateTaskDependencies(ctx context.Context, dependencyIDs []string) error {
	return store.ErrNotImplemented
}
func (s *Store) VerifyCredential(context.Context, string) (core.AuthenticatedCredential, error) {
	return zero[core.AuthenticatedCredential](), store.ErrNotImplemented
}
func (s *Store) VerifyDashboardSession(context.Context, string) (core.AuthenticatedCredential, error) {
	return zero[core.AuthenticatedCredential](), store.ErrNotImplemented
}
func (s *Store) VerifyPersonalAccessToken(context.Context, string) (core.IdentityUser, error) {
	return zero[core.IdentityUser](), store.ErrNotImplemented
}
func (s *Store) WithMonitorSignalClassLock(context.Context, string, monitor.SignalKind, func(context.Context) error) error {
	return store.ErrNotImplemented
}
func (s *Store) WithPlanningSessionFinalization(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	return store.ErrNotImplemented
}
func (s *Store) WithPlanningSessionRun(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	return store.ErrNotImplemented
}

func zero[T any]() T { var value T; return value }
