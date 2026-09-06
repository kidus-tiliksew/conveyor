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

func (s *Store) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	return store.ErrNotImplemented
}
func (s *Store) AddTaskDependency(ctx context.Context, request store.DependencyAdditionRequest) (store.DependencyAdditionResult, error) {
	return zero[store.DependencyAdditionResult](), store.ErrNotImplemented
}
func (s *Store) AdvanceTaskRefreshHead(ctx context.Context, id, newHeadSHA string) error {
	return store.ErrNotImplemented
}
func (s *Store) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, taskID string, now time.Time) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	return store.ErrNotImplemented
}
func (s *Store) ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error) {
	return zero[[]core.Task](), store.ErrNotImplemented
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
func (s *Store) ConfirmTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return zero[core.TaskContextProposal](), store.ErrNotImplemented
}
func (s *Store) ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error) {
	return zero[core.WorkerPairing](), store.ErrNotImplemented
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
func (s *Store) CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	return store.ErrNotImplemented
}
func (s *Store) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	return zero[core.SpecVersion](), store.ErrNotImplemented
}
func (s *Store) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error) {
	return zero[bool](), store.ErrNotImplemented
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
func (s *Store) DeleteWorkspaceForgeToken(context.Context, string) error {
	return store.ErrNotImplemented
}
func (s *Store) DismissTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return zero[core.TaskContextProposal](), store.ErrNotImplemented
}
func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return store.ErrNotImplemented
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
func (s *Store) GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error) {
	return zero[core.ReviewPublication](), store.ErrNotImplemented
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
func (s *Store) LinkTask(context.Context, string, string, string) (monitor.ObservationRecord, error) {
	return zero[monitor.ObservationRecord](), store.ErrNotImplemented
}
func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	return zero[[]core.Artifact](), store.ErrNotImplemented
}
func (s *Store) ListBlockingTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	return zero[[]string](), store.ErrNotImplemented
}
func (s *Store) ListDependencyBlockers(ctx context.Context, taskIDs []string) (map[string]store.DependencyBlockers, error) {
	return zero[map[string]store.DependencyBlockers](), store.ErrNotImplemented
}
func (s *Store) ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error) {
	return zero[[]string](), store.ErrNotImplemented
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
func (s *Store) ListOwnPersonalAccessTokens(context.Context, string) ([]core.PersonalAccessToken, error) {
	return zero[[]core.PersonalAccessToken](), store.ErrNotImplemented
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
func (s *Store) PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.WorkOrderPreemptRequest) (store.WorkOrderPreemptResult, error) {
	return zero[store.WorkOrderPreemptResult](), store.ErrNotImplemented
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
func zero[T any]() T { var value T; return value }
