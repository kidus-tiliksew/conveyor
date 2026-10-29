package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// taskDetailCountingStore counts every store.Store method a request calls,
// so a call vector covers the whole store surface rather than a chosen
// subset (component-http-api, Request-scoped detail reads). The method set
// below is mechanical: one counted pass-through per store.Store method.
type taskDetailCountingStore struct {
	store.Store
	mu    sync.Mutex
	calls map[string]int
}

func newTaskDetailCountingStore(inner store.Store) *taskDetailCountingStore {
	return &taskDetailCountingStore{Store: inner, calls: map[string]int{}}
}

func (c *taskDetailCountingStore) count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

func (c *taskDetailCountingStore) reset() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = map[string]int{}
	return out
}

func (c *taskDetailCountingStore) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	c.count("ReadVerificationReviews")
	return c.Store.(store.VerificationReviewBatchReader).ReadVerificationReviews(ctx, refs)
}

func (c *taskDetailCountingStore) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	c.count("ReadVerificationReview")
	return c.Store.(store.VerificationReviewReader).ReadVerificationReview(ctx, taskID, orderID)
}

func (c *taskDetailCountingStore) AbandonPlanningSession(a0 context.Context, a1 string, a2 ...string) (core.PlanningSession, error) {
	c.count("AbandonPlanningSession")
	return c.Store.AbandonPlanningSession(a0, a1, a2...)
}

func (c *taskDetailCountingStore) AcceptReviewDecisionCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.ReviewDecision) error {
	c.count("AcceptReviewDecisionCommand")
	return c.Store.AcceptReviewDecisionCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) AcknowledgeRequirementStaleness(a0 context.Context, a1 core.RequirementStalenessAcknowledgment) (core.RequirementStalenessAcknowledgment, error) {
	c.count("AcknowledgeRequirementStaleness")
	return c.Store.AcknowledgeRequirementStaleness(a0, a1)
}

func (c *taskDetailCountingStore) AddReviewSeatsCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	c.count("AddReviewSeatsCommand")
	return c.Store.AddReviewSeatsCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) AddTaskDependency(a0 context.Context, a1 store.DependencyAdditionRequest) (store.DependencyAdditionResult, error) {
	c.count("AddTaskDependency")
	return c.Store.AddTaskDependency(a0, a1)
}

func (c *taskDetailCountingStore) AdvanceTaskRefreshHead(a0 context.Context, a1 string, a2 string) error {
	c.count("AdvanceTaskRefreshHead")
	return c.Store.AdvanceTaskRefreshHead(a0, a1, a2)
}

func (c *taskDetailCountingStore) AppendEvent(a0 context.Context, a1 core.Event) error {
	c.count("AppendEvent")
	return c.Store.AppendEvent(a0, a1)
}

func (c *taskDetailCountingStore) AppendPlanningMessage(a0 context.Context, a1 core.PlanningMessage) (core.PlanningMessage, error) {
	c.count("AppendPlanningMessage")
	return c.Store.AppendPlanningMessage(a0, a1)
}

func (c *taskDetailCountingStore) ApplyTaskCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 taskops.Command) (core.Task, error) {
	c.count("ApplyTaskCommand")
	return c.Store.ApplyTaskCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ApplyWorkOrderClock(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 time.Time) (int, error) {
	c.count("ApplyWorkOrderClock")
	return c.Store.ApplyWorkOrderClock(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ApprovePlanningBundle(a0 context.Context, a1 string) (core.PlanningBundle, error) {
	c.count("ApprovePlanningBundle")
	return c.Store.ApprovePlanningBundle(a0, a1)
}

func (c *taskDetailCountingStore) ApproveSpecVersion(a0 context.Context, a1 string, a2 int) error {
	c.count("ApproveSpecVersion")
	return c.Store.ApproveSpecVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) ApproveSpecVersionAndMaterialize(a0 context.Context, a1 string, a2 int) ([]core.Task, error) {
	c.count("ApproveSpecVersionAndMaterialize")
	return c.Store.ApproveSpecVersionAndMaterialize(a0, a1, a2)
}

func (c *taskDetailCountingStore) ArchiveRequirement(a0 context.Context, a1 string, a2 string, a3 []string) error {
	c.count("ArchiveRequirement")
	return c.Store.ArchiveRequirement(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ArchiveSystemDesign(a0 context.Context, a1 string, a2 string, a3 []string) error {
	c.count("ArchiveSystemDesign")
	return c.Store.ArchiveSystemDesign(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) AttachSubmissionGovernance(a0 context.Context, a1 string, a2 string, a3 []string, a4 store.SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	c.count("AttachSubmissionGovernance")
	return c.Store.AttachSubmissionGovernance(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) AttachTaskBranch(a0 context.Context, a1 string, a2 string) (core.Task, error) {
	c.count("AttachTaskBranch")
	return c.Store.AttachTaskBranch(a0, a1, a2)
}

func (c *taskDetailCountingStore) AuthenticateWorker(a0 context.Context, a1 string) (core.Worker, error) {
	c.count("AuthenticateWorker")
	return c.Store.AuthenticateWorker(a0, a1)
}

func (c *taskDetailCountingStore) AutomaticPublicationRetryCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.AutomaticPublicationRetry, a3 time.Time) (bool, error) {
	c.count("AutomaticPublicationRetryCommand")
	return c.Store.AutomaticPublicationRetryCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) BindTaskApproval(a0 context.Context, a1 string, a2 string) error {
	c.count("BindTaskApproval")
	return c.Store.BindTaskApproval(a0, a1, a2)
}

func (c *taskDetailCountingStore) CancelPlanRevisionWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 string) (core.WorkOrder, error) {
	c.count("CancelPlanRevisionWorkOrderCommand")
	return c.Store.CancelPlanRevisionWorkOrderCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) CancelTaskCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.Intervention) (core.Task, error) {
	c.count("CancelTaskCommand")
	return c.Store.CancelTaskCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) ChangeTaskPolicyCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.SetupChangeRequest) (store.SetupChangeResult, error) {
	c.count("ChangeTaskPolicyCommand")
	return c.Store.ChangeTaskPolicyCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) ClaimBaseConflictCheck(a0 context.Context, a1 string, a2 time.Time) (bool, error) {
	c.count("ClaimBaseConflictCheck")
	return c.Store.ClaimBaseConflictCheck(a0, a1, a2)
}

func (c *taskDetailCountingStore) ClaimWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 core.WorkOrderClaim) (core.WorkOrder, error) {
	c.count("ClaimWorkOrderCommand")
	return c.Store.ClaimWorkOrderCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ConfirmDecision(a0 context.Context, a1 string) (core.Decision, error) {
	c.count("ConfirmDecision")
	return c.Store.ConfirmDecision(a0, a1)
}

func (c *taskDetailCountingStore) ConfirmRequirementServes(a0 context.Context, a1 string, a2 string) (core.RequirementServesLink, error) {
	c.count("ConfirmRequirementServes")
	return c.Store.ConfirmRequirementServes(a0, a1, a2)
}

func (c *taskDetailCountingStore) ConfirmRequirementVersion(a0 context.Context, a1 string, a2 int, a3 ...int) (core.Requirement, core.RequirementVersion, error) {
	c.count("ConfirmRequirementVersion")
	return c.Store.ConfirmRequirementVersion(a0, a1, a2, a3...)
}

func (c *taskDetailCountingStore) ConfirmSystemDesignVersion(a0 context.Context, a1 string, a2 int, a3 ...int) (core.SystemDesign, core.SystemDesignVersion, error) {
	c.count("ConfirmSystemDesignVersion")
	return c.Store.ConfirmSystemDesignVersion(a0, a1, a2, a3...)
}

func (c *taskDetailCountingStore) ConfirmTaskContextProposal(a0 context.Context, a1 string, a2 core.TaskContextProposalTargetKind, a3 string) (core.TaskContextProposal, error) {
	c.count("ConfirmTaskContextProposal")
	return c.Store.ConfirmTaskContextProposal(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ConsumeWorkerPairing(a0 context.Context, a1 string, a2 time.Time) (core.WorkerPairing, error) {
	c.count("ConsumeWorkerPairing")
	return c.Store.ConsumeWorkerPairing(a0, a1, a2)
}

func (c *taskDetailCountingStore) CountEvents(a0 context.Context, a1 string, a2 string) (int, error) {
	c.count("CountEvents")
	return c.Store.CountEvents(a0, a1, a2)
}

func (c *taskDetailCountingStore) CountEventsSinceHumanIntervention(a0 context.Context, a1 string, a2 string) (int, error) {
	c.count("CountEventsSinceHumanIntervention")
	return c.Store.CountEventsSinceHumanIntervention(a0, a1, a2)
}

func (c *taskDetailCountingStore) CountOperatorAttention(a0 context.Context) (int, error) {
	c.count("CountOperatorAttention")
	return c.Store.CountOperatorAttention(a0)
}

func (c *taskDetailCountingStore) CreateArtifact(a0 context.Context, a1 core.Artifact, a2 []uint8) (core.Artifact, error) {
	c.count("CreateArtifact")
	return c.Store.CreateArtifact(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateClaimedVerificationEvidence(a0 context.Context, a1 store.ClaimedVerificationEvidenceRequest, a2 []uint8) (core.Artifact, error) {
	c.count("CreateClaimedVerificationEvidence")
	return c.Store.CreateClaimedVerificationEvidence(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateConflictFixCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.ConflictFixRequest) (store.ConflictFixResult, error) {
	c.count("CreateConflictFixCommand")
	return c.Store.CreateConflictFixCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateIntervention(a0 context.Context, a1 core.Intervention) error {
	c.count("CreateIntervention")
	return c.Store.CreateIntervention(a0, a1)
}

func (c *taskDetailCountingStore) CreateJob(a0 context.Context, a1 core.Job) error {
	c.count("CreateJob")
	return c.Store.CreateJob(a0, a1)
}

func (c *taskDetailCountingStore) CreatePlanningBundle(a0 context.Context, a1 core.PlanningBundle) (core.PlanningBundle, error) {
	c.count("CreatePlanningBundle")
	return c.Store.CreatePlanningBundle(a0, a1)
}

func (c *taskDetailCountingStore) CreatePlanningSession(a0 context.Context, a1 core.PlanningSession) (core.PlanningSession, error) {
	c.count("CreatePlanningSession")
	return c.Store.CreatePlanningSession(a0, a1)
}

func (c *taskDetailCountingStore) CreateReferenceDocument(a0 context.Context, a1 core.ReferenceDocument, a2 core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	c.count("CreateReferenceDocument")
	return c.Store.CreateReferenceDocument(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateRequirement(a0 context.Context, a1 core.Requirement, a2 core.RequirementVersion) (core.Requirement, core.RequirementVersion, error) {
	c.count("CreateRequirement")
	return c.Store.CreateRequirement(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateReviewRoundCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 store.ReviewRoundSeatResolution, a4 []core.Job, a5 []core.WorkOrder) error {
	c.count("CreateReviewRoundCommand")
	return c.Store.CreateReviewRoundCommand(a0, a1, a2, a3, a4, a5)
}

func (c *taskDetailCountingStore) CreateSpecVersion(a0 context.Context, a1 core.SpecVersion) (core.SpecVersion, error) {
	c.count("CreateSpecVersion")
	return c.Store.CreateSpecVersion(a0, a1)
}

func (c *taskDetailCountingStore) CreateStageWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.Job, a3 core.WorkOrder) (bool, error) {
	c.count("CreateStageWorkOrderCommand")
	return c.Store.CreateStageWorkOrderCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) CreateSystemDesign(a0 context.Context, a1 core.SystemDesign, a2 core.SystemDesignVersion) (core.SystemDesign, core.SystemDesignVersion, error) {
	c.count("CreateSystemDesign")
	return c.Store.CreateSystemDesign(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateTask(a0 context.Context, a1 core.Task) error {
	c.count("CreateTask")
	return c.Store.CreateTask(a0, a1)
}

func (c *taskDetailCountingStore) CreateTaskWithAttachments(a0 context.Context, a1 core.Task, a2 []string, a3 store.TaskContextInput, a4 []store.ArtifactUpload) error {
	c.count("CreateTaskWithAttachments")
	return c.Store.CreateTaskWithAttachments(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) CreateTaskWithDependencies(a0 context.Context, a1 core.Task, a2 []string) error {
	c.count("CreateTaskWithDependencies")
	return c.Store.CreateTaskWithDependencies(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateTaskWithDependenciesAndContext(a0 context.Context, a1 core.Task, a2 []string, a3 store.TaskContextInput) error {
	c.count("CreateTaskWithDependenciesAndContext")
	return c.Store.CreateTaskWithDependenciesAndContext(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) CreateWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.WorkOrder) error {
	c.count("CreateWorkOrderCommand")
	return c.Store.CreateWorkOrderCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) CreateWorker(a0 context.Context, a1 core.Worker) error {
	c.count("CreateWorker")
	return c.Store.CreateWorker(a0, a1)
}

func (c *taskDetailCountingStore) CreateWorkerPairing(a0 context.Context, a1 core.WorkerPairing) error {
	c.count("CreateWorkerPairing")
	return c.Store.CreateWorkerPairing(a0, a1)
}

func (c *taskDetailCountingStore) DeleteReferenceDocument(a0 context.Context, a1 string) error {
	c.count("DeleteReferenceDocument")
	return c.Store.DeleteReferenceDocument(a0, a1)
}

func (c *taskDetailCountingStore) DismissDecision(a0 context.Context, a1 string) (core.Decision, error) {
	c.count("DismissDecision")
	return c.Store.DismissDecision(a0, a1)
}

func (c *taskDetailCountingStore) DismissDecisionSupersessionSweep(a0 context.Context, a1 string, a2 string, a3 string) (core.DecisionSupersessionSweepEntry, error) {
	c.count("DismissDecisionSupersessionSweep")
	return c.Store.DismissDecisionSupersessionSweep(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) DismissRequirementServes(a0 context.Context, a1 string, a2 string) (core.RequirementServesLink, error) {
	c.count("DismissRequirementServes")
	return c.Store.DismissRequirementServes(a0, a1, a2)
}

func (c *taskDetailCountingStore) DismissRequirementVersion(a0 context.Context, a1 string, a2 int) (core.Requirement, core.RequirementVersion, error) {
	c.count("DismissRequirementVersion")
	return c.Store.DismissRequirementVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) DismissSystemDesignVersion(a0 context.Context, a1 string, a2 int) (core.SystemDesign, core.SystemDesignVersion, error) {
	c.count("DismissSystemDesignVersion")
	return c.Store.DismissSystemDesignVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) DismissTaskContextProposal(a0 context.Context, a1 string, a2 core.TaskContextProposalTargetKind, a3 string) (core.TaskContextProposal, error) {
	c.count("DismissTaskContextProposal")
	return c.Store.DismissTaskContextProposal(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) EnsureTaskEnqueued(a0 context.Context, a1 string) error {
	c.count("EnsureTaskEnqueued")
	return c.Store.EnsureTaskEnqueued(a0, a1)
}

func (c *taskDetailCountingStore) FinalizePlanningSession(a0 context.Context, a1 store.PlanningFinalizeRequest) (core.PlanningSession, error) {
	c.count("FinalizePlanningSession")
	return c.Store.FinalizePlanningSession(a0, a1)
}

func (c *taskDetailCountingStore) FinalizeWorkOrderAttemptObservability(a0 context.Context, a1 string, a2 string, a3 core.WorkOrderAttemptCheckpoint) error {
	c.count("FinalizeWorkOrderAttemptObservability")
	return c.Store.FinalizeWorkOrderAttemptObservability(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) GetApprovedSpecVersion(a0 context.Context, a1 string) (core.SpecVersion, bool, error) {
	c.count("GetApprovedSpecVersion")
	return c.Store.GetApprovedSpecVersion(a0, a1)
}

func (c *taskDetailCountingStore) GetArtifact(a0 context.Context, a1 string) (core.Artifact, []uint8, error) {
	c.count("GetArtifact")
	return c.Store.GetArtifact(a0, a1)
}

func (c *taskDetailCountingStore) GetArtifactForPlanningSession(a0 context.Context, a1 string, a2 string) (core.Artifact, []uint8, error) {
	c.count("GetArtifactForPlanningSession")
	return c.Store.GetArtifactForPlanningSession(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetDecision(a0 context.Context, a1 string) (core.Decision, error) {
	c.count("GetDecision")
	return c.Store.GetDecision(a0, a1)
}

func (c *taskDetailCountingStore) GetDocumentationGateEvidence(a0 context.Context, a1 string, a2 string) (core.DocumentationGateEvidence, error) {
	c.count("GetDocumentationGateEvidence")
	return c.Store.GetDocumentationGateEvidence(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetGitHubLifecycle(a0 context.Context, a1 string) (core.GitHubLifecycle, bool, error) {
	c.count("GetGitHubLifecycle")
	return c.Store.GetGitHubLifecycle(a0, a1)
}

func (c *taskDetailCountingStore) GetLatestJob(a0 context.Context, a1 string) (core.Job, bool, error) {
	c.count("GetLatestJob")
	return c.Store.GetLatestJob(a0, a1)
}

func (c *taskDetailCountingStore) GetLatestSpecVersion(a0 context.Context, a1 string) (core.SpecVersion, bool, error) {
	c.count("GetLatestSpecVersion")
	return c.Store.GetLatestSpecVersion(a0, a1)
}

func (c *taskDetailCountingStore) GetPlanningBundle(a0 context.Context, a1 string) (core.PlanningBundle, error) {
	c.count("GetPlanningBundle")
	return c.Store.GetPlanningBundle(a0, a1)
}

func (c *taskDetailCountingStore) GetPlanningSession(a0 context.Context, a1 string) (core.PlanningSession, error) {
	c.count("GetPlanningSession")
	return c.Store.GetPlanningSession(a0, a1)
}

func (c *taskDetailCountingStore) GetPullRequestClose(a0 context.Context, a1 string) (core.PullRequestClose, bool, error) {
	c.count("GetPullRequestClose")
	return c.Store.GetPullRequestClose(a0, a1)
}

func (c *taskDetailCountingStore) GetReferenceDocument(a0 context.Context, a1 string) (core.ReferenceDocument, error) {
	c.count("GetReferenceDocument")
	return c.Store.GetReferenceDocument(a0, a1)
}

func (c *taskDetailCountingStore) GetReferenceDocumentVersion(a0 context.Context, a1 string, a2 int) (core.ReferenceDocumentVersion, error) {
	c.count("GetReferenceDocumentVersion")
	return c.Store.GetReferenceDocumentVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetRequirement(a0 context.Context, a1 string) (core.Requirement, error) {
	c.count("GetRequirement")
	return c.Store.GetRequirement(a0, a1)
}

func (c *taskDetailCountingStore) GetRequirementInsightHeadline(a0 context.Context, a1 string) (store.RequirementInsightHeadline, error) {
	c.count("GetRequirementInsightHeadline")
	return c.Store.GetRequirementInsightHeadline(a0, a1)
}

func (c *taskDetailCountingStore) GetRequirementVersion(a0 context.Context, a1 string, a2 int) (core.RequirementVersion, error) {
	c.count("GetRequirementVersion")
	return c.Store.GetRequirementVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetReviewPublication(a0 context.Context, a1 string) (core.ReviewPublication, error) {
	c.count("GetReviewPublication")
	return c.Store.GetReviewPublication(a0, a1)
}

func (c *taskDetailCountingStore) GetSpecVersion(a0 context.Context, a1 string, a2 int) (core.SpecVersion, bool, error) {
	c.count("GetSpecVersion")
	return c.Store.GetSpecVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetSystemDesign(a0 context.Context, a1 string) (core.SystemDesign, error) {
	c.count("GetSystemDesign")
	return c.Store.GetSystemDesign(a0, a1)
}

func (c *taskDetailCountingStore) GetSystemDesignVersion(a0 context.Context, a1 string, a2 int) (core.SystemDesignVersion, error) {
	c.count("GetSystemDesignVersion")
	return c.Store.GetSystemDesignVersion(a0, a1, a2)
}

func (c *taskDetailCountingStore) GetTask(a0 context.Context, a1 string) (core.Task, error) {
	c.count("GetTask")
	return c.Store.GetTask(a0, a1)
}

func (c *taskDetailCountingStore) GetTaskByIntakeKey(a0 context.Context, a1 string) (core.Task, bool, error) {
	c.count("GetTaskByIntakeKey")
	return c.Store.GetTaskByIntakeKey(a0, a1)
}

func (c *taskDetailCountingStore) GetTranscript(a0 context.Context, a1 string) (core.Transcript, error) {
	c.count("GetTranscript")
	return c.Store.GetTranscript(a0, a1)
}

func (c *taskDetailCountingStore) GetWorkOrder(a0 context.Context, a1 string) (core.WorkOrder, error) {
	c.count("GetWorkOrder")
	return c.Store.GetWorkOrder(a0, a1)
}

func (c *taskDetailCountingStore) GetWorkOrderActivitySnapshot(a0 context.Context, a1 string) (core.WorkOrderActivitySnapshot, bool, error) {
	c.count("GetWorkOrderActivitySnapshot")
	return c.Store.GetWorkOrderActivitySnapshot(a0, a1)
}

func (c *taskDetailCountingStore) HeartbeatWorker(a0 context.Context, a1 string, a2 time.Time, a3 []core.HarnessProbe) (core.Worker, error) {
	c.count("HeartbeatWorker")
	return c.Store.HeartbeatWorker(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) IsDurable() bool {
	c.count("IsDurable")
	return c.Store.IsDurable()
}

func (c *taskDetailCountingStore) LineageNodeExists(a0 context.Context, a1 core.LineageNode) (bool, error) {
	c.count("LineageNodeExists")
	return c.Store.LineageNodeExists(a0, a1)
}

func (c *taskDetailCountingStore) ListActiveSystemDesignDriftCounts(a0 context.Context) (map[string]int, error) {
	c.count("ListActiveSystemDesignDriftCounts")
	return c.Store.ListActiveSystemDesignDriftCounts(a0)
}

func (c *taskDetailCountingStore) ListActivityMarkers(a0 context.Context) ([]store.ActivityMarker, error) {
	c.count("ListActivityMarkers")
	return c.Store.ListActivityMarkers(a0)
}

func (c *taskDetailCountingStore) ListActivityMarkersForTasks(a0 context.Context, a1 []string) ([]store.ActivityMarker, error) {
	c.count("ListActivityMarkersForTasks")
	return c.Store.ListActivityMarkersForTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListArtifacts(a0 context.Context) ([]core.Artifact, error) {
	c.count("ListArtifacts")
	return c.Store.ListArtifacts(a0)
}

func (c *taskDetailCountingStore) ListArtifactsForLineage(a0 context.Context, a1 []core.LineageNode) ([]core.Artifact, error) {
	c.count("ListArtifactsForLineage")
	return c.Store.ListArtifactsForLineage(a0, a1)
}

func (c *taskDetailCountingStore) ListAutomaticPublicationRetries(a0 context.Context, a1 time.Time) ([]store.AutomaticPublicationRetry, error) {
	c.count("ListAutomaticPublicationRetries")
	return c.Store.ListAutomaticPublicationRetries(a0, a1)
}

func (c *taskDetailCountingStore) ListAutomaticReviewEvents(a0 context.Context, a1 []string) (map[string][]core.Event, error) {
	c.count("ListAutomaticReviewEvents")
	return c.Store.ListAutomaticReviewEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListBlockingTaskIDs(a0 context.Context, a1 string) ([]string, error) {
	c.count("ListBlockingTaskIDs")
	return c.Store.ListBlockingTaskIDs(a0, a1)
}

func (c *taskDetailCountingStore) ListCallerAttentionTaskPage(a0 context.Context, a1 store.CallerAttentionQuery) (store.TaskPage, error) {
	c.count("ListCallerAttentionTaskPage")
	return c.Store.ListCallerAttentionTaskPage(a0, a1)
}

func (c *taskDetailCountingStore) ListCheckpointContextCandidates(a0 context.Context, a1 string) ([]store.CheckpointContextCandidate, error) {
	c.count("ListCheckpointContextCandidates")
	return c.Store.ListCheckpointContextCandidates(a0, a1)
}

func (c *taskDetailCountingStore) ListClaimBlockingProposalsForTask(a0 context.Context, a1 string) ([]store.ClaimBlockingProposal, error) {
	c.count("ListClaimBlockingProposalsForTask")
	return c.Store.ListClaimBlockingProposalsForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListClaimBlockingProposalsForTasks(a0 context.Context, a1 []string) (map[string][]store.ClaimBlockingProposal, error) {
	c.count("ListClaimBlockingProposalsForTasks")
	return c.Store.ListClaimBlockingProposalsForTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListDecisions(a0 context.Context) ([]core.Decision, error) {
	c.count("ListDecisions")
	return c.Store.ListDecisions(a0)
}

func (c *taskDetailCountingStore) ListDependencyBlockers(a0 context.Context, a1 []string) (map[string]store.DependencyBlockers, error) {
	c.count("ListDependencyBlockers")
	return c.Store.ListDependencyBlockers(a0, a1)
}

func (c *taskDetailCountingStore) ListDependentTaskIDs(a0 context.Context, a1 string) ([]string, error) {
	c.count("ListDependentTaskIDs")
	return c.Store.ListDependentTaskIDs(a0, a1)
}

func (c *taskDetailCountingStore) ListDocumentEventPage(a0 context.Context, a1 core.LineageNodeType, a2 string, a3 store.DocumentEventQuery) (store.DocumentEventPage, error) {
	c.count("ListDocumentEventPage")
	return c.Store.ListDocumentEventPage(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ListDocumentHeads(a0 context.Context, a1 []string) (map[string]store.DocumentHead, error) {
	c.count("ListDocumentHeads")
	return c.Store.ListDocumentHeads(a0, a1)
}

func (c *taskDetailCountingStore) ListDocumentMetadata(a0 context.Context, a1 []string) (store.DocumentMetadataSet, error) {
	c.count("ListDocumentMetadata")
	return c.Store.ListDocumentMetadata(a0, a1)
}

func (c *taskDetailCountingStore) ListDocumentOperatorNotesForTask(a0 context.Context, a1 string) ([]core.OperatorNote, error) {
	c.count("ListDocumentOperatorNotesForTask")
	return c.Store.ListDocumentOperatorNotesForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListElapsedWorkOrderTaskIDs(a0 context.Context, a1 time.Time) ([]string, error) {
	c.count("ListElapsedWorkOrderTaskIDs")
	return c.Store.ListElapsedWorkOrderTaskIDs(a0, a1)
}

func (c *taskDetailCountingStore) ListEvents(a0 context.Context, a1 string) ([]core.Event, error) {
	c.count("ListEvents")
	return c.Store.ListEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListEventsAfter(a0 context.Context, a1 string, a2 int64) ([]core.Event, error) {
	c.count("ListEventsAfter")
	return c.Store.ListEventsAfter(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListFactoryActs(a0 context.Context, a1 time.Time, a2 int) ([]nextaction.Act, error) {
	c.count("ListFactoryActs")
	return c.Store.ListFactoryActs(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListGitHubLifecycleReconcileTaskIDs(a0 context.Context) ([]string, error) {
	c.count("ListGitHubLifecycleReconcileTaskIDs")
	return c.Store.ListGitHubLifecycleReconcileTaskIDs(a0)
}

func (c *taskDetailCountingStore) ListGovernanceDesigns(a0 context.Context, a1 string) ([]core.GovernanceDesignContext, error) {
	c.count("ListGovernanceDesigns")
	return c.Store.ListGovernanceDesigns(a0, a1)
}

func (c *taskDetailCountingStore) ListHarnessModelFailures(a0 context.Context) ([]core.HarnessModelFailure, error) {
	c.count("ListHarnessModelFailures")
	return c.Store.ListHarnessModelFailures(a0)
}

func (c *taskDetailCountingStore) ListInterventions(a0 context.Context, a1 string) ([]core.Intervention, error) {
	c.count("ListInterventions")
	return c.Store.ListInterventions(a0, a1)
}

func (c *taskDetailCountingStore) ListJobs(a0 context.Context, a1 string) ([]core.Job, error) {
	c.count("ListJobs")
	return c.Store.ListJobs(a0, a1)
}

func (c *taskDetailCountingStore) ListLatestSpecVersionNumbers(a0 context.Context, a1 []string) (map[string]int, error) {
	c.count("ListLatestSpecVersionNumbers")
	return c.Store.ListLatestSpecVersionNumbers(a0, a1)
}

func (c *taskDetailCountingStore) ListLineageContextRecords(a0 context.Context, a1 []core.LineageNode) (store.LineageContextRecords, error) {
	c.count("ListLineageContextRecords")
	return c.Store.ListLineageContextRecords(a0, a1)
}

func (c *taskDetailCountingStore) ListLineageKindLinks(a0 context.Context, a1 core.LineageKindLinkQuery) ([]core.LineageLink, error) {
	c.count("ListLineageKindLinks")
	return c.Store.ListLineageKindLinks(a0, a1)
}

func (c *taskDetailCountingStore) ListLineageLinks(a0 context.Context) ([]core.LineageLink, error) {
	c.count("ListLineageLinks")
	return c.Store.ListLineageLinks(a0)
}

func (c *taskDetailCountingStore) ListLineageNeighborhood(a0 context.Context, a1 []core.LineageNode, a2 core.LineageTraversalBudget) ([]core.LineageLink, error) {
	c.count("ListLineageNeighborhood")
	return c.Store.ListLineageNeighborhood(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListLineageNodeRecords(a0 context.Context, a1 []core.LineageNode) (map[core.LineageNode]store.LineageNodeRecord, error) {
	c.count("ListLineageNodeRecords")
	return c.Store.ListLineageNodeRecords(a0, a1)
}

func (c *taskDetailCountingStore) ListLineageNodeStatuses(a0 context.Context, a1 []core.LineageNode) (map[core.LineageNode]core.LineageNodeStatus, error) {
	c.count("ListLineageNodeStatuses")
	return c.Store.ListLineageNodeStatuses(a0, a1)
}

func (c *taskDetailCountingStore) ListLiveWorkOrders(a0 context.Context) ([]core.WorkOrder, error) {
	c.count("ListLiveWorkOrders")
	return c.Store.ListLiveWorkOrders(a0)
}

func (c *taskDetailCountingStore) ListMonitorPullRequestEventsForTasks(a0 context.Context, a1 []string) (map[string][]core.Event, error) {
	c.count("ListMonitorPullRequestEventsForTasks")
	return c.Store.ListMonitorPullRequestEventsForTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListNextActionEvents(a0 context.Context, a1 []string) (map[string][]core.Event, error) {
	c.count("ListNextActionEvents")
	return c.Store.ListNextActionEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListNextActionHistory(a0 context.Context, a1 []string) (map[string]store.NextActionHistory, error) {
	c.count("ListNextActionHistory")
	return c.Store.ListNextActionHistory(a0, a1)
}

func (c *taskDetailCountingStore) ListOpenDependents(a0 context.Context, a1 []string) ([]store.TaskHead, error) {
	c.count("ListOpenDependents")
	return c.Store.ListOpenDependents(a0, a1)
}

func (c *taskDetailCountingStore) ListPendingAuthorityProposalsForTask(a0 context.Context, a1 string) ([]core.PendingProposal, error) {
	c.count("ListPendingAuthorityProposalsForTask")
	return c.Store.ListPendingAuthorityProposalsForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListPendingProposals(a0 context.Context) ([]core.PendingProposal, error) {
	c.count("ListPendingProposals")
	return c.Store.ListPendingProposals(a0)
}

func (c *taskDetailCountingStore) ListPendingSystemDesignVersionsForTask(a0 context.Context, a1 string) ([]core.SystemDesignVersion, error) {
	c.count("ListPendingSystemDesignVersionsForTask")
	return c.Store.ListPendingSystemDesignVersionsForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListPlanningBundles(a0 context.Context) ([]core.PlanningBundle, error) {
	c.count("ListPlanningBundles")
	return c.Store.ListPlanningBundles(a0)
}

func (c *taskDetailCountingStore) ListPlanningMessages(a0 context.Context, a1 string) ([]core.PlanningMessage, error) {
	c.count("ListPlanningMessages")
	return c.Store.ListPlanningMessages(a0, a1)
}

func (c *taskDetailCountingStore) ListPlanningSessions(a0 context.Context) ([]core.PlanningSession, error) {
	c.count("ListPlanningSessions")
	return c.Store.ListPlanningSessions(a0)
}

func (c *taskDetailCountingStore) ListReferenceDocumentEvents(a0 context.Context, a1 string) ([]core.Event, error) {
	c.count("ListReferenceDocumentEvents")
	return c.Store.ListReferenceDocumentEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListReferenceDocumentVersions(a0 context.Context, a1 string) ([]core.ReferenceDocumentVersion, error) {
	c.count("ListReferenceDocumentVersions")
	return c.Store.ListReferenceDocumentVersions(a0, a1)
}

func (c *taskDetailCountingStore) ListReferenceDocuments(a0 context.Context, a1 bool) ([]core.ReferenceDocument, error) {
	c.count("ListReferenceDocuments")
	return c.Store.ListReferenceDocuments(a0, a1)
}

func (c *taskDetailCountingStore) ListRepositoryInstallTasks(a0 context.Context, a1 string) ([]core.RepositoryInstallTask, error) {
	c.count("ListRepositoryInstallTasks")
	return c.Store.ListRepositoryInstallTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListRequirementDeliveryEventsForTasks(a0 context.Context, a1 []string) (map[string][]core.Event, error) {
	c.count("ListRequirementDeliveryEventsForTasks")
	return c.Store.ListRequirementDeliveryEventsForTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListRequirementDeliveryLineage(a0 context.Context, a1 string, a2 core.LineageTraversalBudget) ([]core.LineageLink, error) {
	c.count("ListRequirementDeliveryLineage")
	return c.Store.ListRequirementDeliveryLineage(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListRequirementDeliveryLineageByRequirement(a0 context.Context, a1 []string, a2 core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	c.count("ListRequirementDeliveryLineageByRequirement")
	return c.Store.ListRequirementDeliveryLineageByRequirement(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListRequirementEvents(a0 context.Context, a1 string) ([]core.Event, error) {
	c.count("ListRequirementEvents")
	return c.Store.ListRequirementEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListRequirementEventsByRequirement(a0 context.Context) (map[string][]core.Event, error) {
	c.count("ListRequirementEventsByRequirement")
	return c.Store.ListRequirementEventsByRequirement(a0)
}

func (c *taskDetailCountingStore) ListRequirementServes(a0 context.Context) ([]core.RequirementServesLink, error) {
	c.count("ListRequirementServes")
	return c.Store.ListRequirementServes(a0)
}

func (c *taskDetailCountingStore) ListRequirementVersions(a0 context.Context, a1 string) ([]core.RequirementVersion, error) {
	c.count("ListRequirementVersions")
	return c.Store.ListRequirementVersions(a0, a1)
}

func (c *taskDetailCountingStore) ListRequirementVersionsByRequirement(a0 context.Context) (map[string][]core.RequirementVersion, error) {
	c.count("ListRequirementVersionsByRequirement")
	return c.Store.ListRequirementVersionsByRequirement(a0)
}

func (c *taskDetailCountingStore) ListRequirements(a0 context.Context, a1 bool) ([]core.Requirement, error) {
	c.count("ListRequirements")
	return c.Store.ListRequirements(a0, a1)
}

func (c *taskDetailCountingStore) ListReviewRoundSeats(a0 context.Context, a1 string) ([]store.ReviewRoundSeats, error) {
	c.count("ListReviewRoundSeats")
	return c.Store.ListReviewRoundSeats(a0, a1)
}

func (c *taskDetailCountingStore) ListSystemDesignEvents(a0 context.Context, a1 string) ([]core.Event, error) {
	c.count("ListSystemDesignEvents")
	return c.Store.ListSystemDesignEvents(a0, a1)
}

func (c *taskDetailCountingStore) ListSystemDesignEventsByDocument(a0 context.Context) (map[string][]core.Event, error) {
	c.count("ListSystemDesignEventsByDocument")
	return c.Store.ListSystemDesignEventsByDocument(a0)
}

func (c *taskDetailCountingStore) ListSystemDesignProposalEventsForTask(a0 context.Context, a1 string) ([]core.Event, error) {
	c.count("ListSystemDesignProposalEventsForTask")
	return c.Store.ListSystemDesignProposalEventsForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListSystemDesignProposalVersionsForTask(a0 context.Context, a1 string) ([]core.SystemDesignVersion, error) {
	c.count("ListSystemDesignProposalVersionsForTask")
	return c.Store.ListSystemDesignProposalVersionsForTask(a0, a1)
}

func (c *taskDetailCountingStore) ListSystemDesignVersions(a0 context.Context, a1 string) ([]core.SystemDesignVersion, error) {
	c.count("ListSystemDesignVersions")
	return c.Store.ListSystemDesignVersions(a0, a1)
}

func (c *taskDetailCountingStore) ListSystemDesignVersionsByDocument(a0 context.Context) (map[string][]core.SystemDesignVersion, error) {
	c.count("ListSystemDesignVersionsByDocument")
	return c.Store.ListSystemDesignVersionsByDocument(a0)
}

func (c *taskDetailCountingStore) ListSystemDesigns(a0 context.Context, a1 bool) ([]core.SystemDesign, error) {
	c.count("ListSystemDesigns")
	return c.Store.ListSystemDesigns(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskActivitySnapshots(a0 context.Context, a1 string, a2 []string) (map[string]core.WorkOrderActivitySnapshot, error) {
	c.count("ListTaskActivitySnapshots")
	return c.Store.ListTaskActivitySnapshots(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListTaskContextDesignPins(a0 context.Context, a1 []string, a2 []string) (map[string]map[string]int, error) {
	c.count("ListTaskContextDesignPins")
	return c.Store.ListTaskContextDesignPins(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListTaskContextProposals(a0 context.Context, a1 string, a2 core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	c.count("ListTaskContextProposals")
	return c.Store.ListTaskContextProposals(a0, a1, a2)
}

func (c *taskDetailCountingStore) ListTaskHeads(a0 context.Context, a1 []string) (map[string]store.TaskHead, error) {
	c.count("ListTaskHeads")
	return c.Store.ListTaskHeads(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskMap(a0 context.Context, a1 store.TaskMapQuery) (store.TaskMap, error) {
	c.count("ListTaskMap")
	return c.Store.ListTaskMap(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskOperations(a0 context.Context, a1 store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	c.count("ListTaskOperations")
	return c.Store.ListTaskOperations(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskPage(a0 context.Context, a1 store.TaskOperationsQuery) (store.TaskPage, error) {
	c.count("ListTaskPage")
	return c.Store.ListTaskPage(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskTranscriptCaptures(a0 context.Context, a1 string) (map[string][]core.WorkOrderTranscriptCapture, error) {
	c.count("ListTaskTranscriptCaptures")
	return c.Store.ListTaskTranscriptCaptures(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskWorkOrders(a0 context.Context, a1 string) ([]core.WorkOrder, error) {
	c.count("ListTaskWorkOrders")
	return c.Store.ListTaskWorkOrders(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskWorkOrdersSnapshot(a0 context.Context, a1 string) ([]core.WorkOrder, error) {
	c.count("ListTaskWorkOrdersSnapshot")
	return c.Store.ListTaskWorkOrdersSnapshot(a0, a1)
}

func (c *taskDetailCountingStore) ListTaskWorkOrdersStored(a0 context.Context, a1 string) ([]core.WorkOrder, error) {
	c.count("ListTaskWorkOrdersStored")
	return c.Store.ListTaskWorkOrdersStored(a0, a1)
}

func (c *taskDetailCountingStore) ListTasks(a0 context.Context) ([]core.Task, error) {
	c.count("ListTasks")
	return c.Store.ListTasks(a0)
}

func (c *taskDetailCountingStore) ListTasksFiltered(a0 context.Context, a1 store.TaskFilter) ([]core.Task, error) {
	c.count("ListTasksFiltered")
	return c.Store.ListTasksFiltered(a0, a1)
}

func (c *taskDetailCountingStore) ListWorkOrderTranscriptCaptures(a0 context.Context, a1 string) ([]core.WorkOrderTranscriptCapture, error) {
	c.count("ListWorkOrderTranscriptCaptures")
	return c.Store.ListWorkOrderTranscriptCaptures(a0, a1)
}

func (c *taskDetailCountingStore) ListWorkOrders(a0 context.Context) ([]core.WorkOrder, error) {
	c.count("ListWorkOrders")
	return c.Store.ListWorkOrders(a0)
}

func (c *taskDetailCountingStore) ListWorkOrdersForTasks(a0 context.Context, a1 []string) ([]core.WorkOrder, error) {
	c.count("ListWorkOrdersForTasks")
	return c.Store.ListWorkOrdersForTasks(a0, a1)
}

func (c *taskDetailCountingStore) ListWorkers(a0 context.Context) ([]core.Worker, error) {
	c.count("ListWorkers")
	return c.Store.ListWorkers(a0)
}

func (c *taskDetailCountingStore) MarkTaskApprovalStale(a0 context.Context, a1 string, a2 string, a3 string, a4 string, a5 string) (bool, error) {
	c.count("MarkTaskApprovalStale")
	return c.Store.MarkTaskApprovalStale(a0, a1, a2, a3, a4, a5)
}

func (c *taskDetailCountingStore) MergeTaskAtGateCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.Intervention, a3 string, a4 core.Event) (core.Task, error) {
	c.count("MergeTaskAtGateCommand")
	return c.Store.MergeTaskAtGateCommand(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) MonitorOriginsForTasks(a0 context.Context, a1 []string) (map[string]monitor.ObservationRecord, error) {
	c.count("MonitorOriginsForTasks")
	return c.Store.MonitorOriginsForTasks(a0, a1)
}

func (c *taskDetailCountingStore) PendingProposalsProjection(a0 context.Context) (store.PendingProposalsProjection, error) {
	c.count("PendingProposalsProjection")
	return c.Store.PendingProposalsProjection(a0)
}

func (c *taskDetailCountingStore) PinPlanningSessionRepo(a0 context.Context, a1 string, a2 string, a3 string) (core.PlanningSession, error) {
	c.count("PinPlanningSessionRepo")
	return c.Store.PinPlanningSessionRepo(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) PreemptWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.WorkOrderPreemptRequest) (store.WorkOrderPreemptResult, error) {
	c.count("PreemptWorkOrderCommand")
	return c.Store.PreemptWorkOrderCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) ProposeDecision(a0 context.Context, a1 core.Decision) (core.Decision, error) {
	c.count("ProposeDecision")
	return c.Store.ProposeDecision(a0, a1)
}

func (c *taskDetailCountingStore) ProposeRequirementServes(a0 context.Context, a1 string, a2 string, a3 core.RequirementServesSource, a4 bool) (core.RequirementServesLink, error) {
	c.count("ProposeRequirementServes")
	return c.Store.ProposeRequirementServes(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) ProposeRequirementVersion(a0 context.Context, a1 core.RequirementVersion) (core.RequirementVersion, error) {
	c.count("ProposeRequirementVersion")
	return c.Store.ProposeRequirementVersion(a0, a1)
}

func (c *taskDetailCountingStore) ProposeSystemDesignVersion(a0 context.Context, a1 core.SystemDesignVersion) (core.SystemDesignVersion, error) {
	c.count("ProposeSystemDesignVersion")
	return c.Store.ProposeSystemDesignVersion(a0, a1)
}

func (c *taskDetailCountingStore) ProposeTaskContext(a0 context.Context, a1 core.TaskContextProposalInput) (core.TaskContextProposal, bool, error) {
	c.count("ProposeTaskContext")
	return c.Store.ProposeTaskContext(a0, a1)
}

func (c *taskDetailCountingStore) QueueGitHubLifecycle(a0 context.Context, a1 core.GitHubLifecycle) error {
	c.count("QueueGitHubLifecycle")
	return c.Store.QueueGitHubLifecycle(a0, a1)
}

func (c *taskDetailCountingStore) QueuePullRequestClose(a0 context.Context, a1 core.PullRequestClose) error {
	c.count("QueuePullRequestClose")
	return c.Store.QueuePullRequestClose(a0, a1)
}

func (c *taskDetailCountingStore) QueueReviewPublication(a0 context.Context, a1 core.ReviewPublication) error {
	c.count("QueueReviewPublication")
	return c.Store.QueueReviewPublication(a0, a1)
}

func (c *taskDetailCountingStore) ReadRequirementInsightInputs(a0 context.Context, a1 string) (insight.Inputs, error) {
	c.count("ReadRequirementInsightInputs")
	return c.Store.ReadRequirementInsightInputs(a0, a1)
}

func (c *taskDetailCountingStore) ReadTaskEventStream(a0 context.Context, a1 store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	c.count("ReadTaskEventStream")
	return c.Store.ReadTaskEventStream(a0, a1)
}

func (c *taskDetailCountingStore) ReadTaskEventWindow(a0 context.Context, a1 store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	c.count("ReadTaskEventWindow")
	return c.Store.ReadTaskEventWindow(a0, a1)
}

func (c *taskDetailCountingStore) RebuildLineage(a0 context.Context, a1 core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	c.count("RebuildLineage")
	return c.Store.RebuildLineage(a0, a1)
}

func (c *taskDetailCountingStore) ReconcileGitHubLifecycles(a0 context.Context) (int, error) {
	c.count("ReconcileGitHubLifecycles")
	return c.Store.ReconcileGitHubLifecycles(a0)
}

func (c *taskDetailCountingStore) ReconcileReviewPublications(a0 context.Context) (int, error) {
	c.count("ReconcileReviewPublications")
	return c.Store.ReconcileReviewPublications(a0)
}

func (c *taskDetailCountingStore) RecordDocumentationGateEvidence(a0 context.Context, a1 core.DocumentationGateEvidence) error {
	c.count("RecordDocumentationGateEvidence")
	return c.Store.RecordDocumentationGateEvidence(a0, a1)
}

func (c *taskDetailCountingStore) RecordPlanningExplorationTokens(a0 context.Context, a1 string, a2 int) (core.PlanningSession, error) {
	c.count("RecordPlanningExplorationTokens")
	return c.Store.RecordPlanningExplorationTokens(a0, a1, a2)
}

func (c *taskDetailCountingStore) RecordReferenceDocumentConsulted(a0 context.Context, a1 string, a2 int, a3 string) error {
	c.count("RecordReferenceDocumentConsulted")
	return c.Store.RecordReferenceDocumentConsulted(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RecordRequirementInsightHeadline(a0 context.Context, a1 store.RequirementInsightHeadline) (store.RequirementInsightHeadline, error) {
	c.count("RecordRequirementInsightHeadline")
	return c.Store.RecordRequirementInsightHeadline(a0, a1)
}

func (c *taskDetailCountingStore) RecordReviewSummaryCommand(a0 context.Context, a1 store.ReviewSummaryRequest) (store.ReviewSummaryResult, error) {
	c.count("RecordReviewSummaryCommand")
	return c.Store.RecordReviewSummaryCommand(a0, a1)
}

func (c *taskDetailCountingStore) RecordSystemDesignConsulted(a0 context.Context, a1 string, a2 int, a3 string, a4 string) error {
	c.count("RecordSystemDesignConsulted")
	return c.Store.RecordSystemDesignConsulted(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) RecordWorkOrderAttemptCapture(a0 context.Context, a1 string, a2 core.WorkOrderClaimIdentity, a3 core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error) {
	c.count("RecordWorkOrderAttemptCapture")
	return c.Store.RecordWorkOrderAttemptCapture(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RecordWorkOrderAttemptCheckpoint(a0 context.Context, a1 string, a2 string, a3 core.WorkOrderAttemptCheckpoint) (bool, error) {
	c.count("RecordWorkOrderAttemptCheckpoint")
	return c.Store.RecordWorkOrderAttemptCheckpoint(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RecordWorkOrderContinuation(a0 context.Context, a1 string, a2 core.WorkOrderClaimIdentity, a3 core.WorkOrderContinuation) (core.WorkOrder, error) {
	c.count("RecordWorkOrderContinuation")
	return c.Store.RecordWorkOrderContinuation(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RecoverInterruptedReviewRoundCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.InterruptedReviewRecoveryRequest, a3 time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	c.count("RecoverInterruptedReviewRoundCommand")
	return c.Store.RecoverInterruptedReviewRoundCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RecoverWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 string, a4 string, a5 time.Duration, a6 ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	c.count("RecoverWorkOrderCommand")
	return c.Store.RecoverWorkOrderCommand(a0, a1, a2, a3, a4, a5, a6...)
}

func (c *taskDetailCountingStore) RedispatchWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 time.Duration) (core.WorkOrder, error) {
	c.count("RedispatchWorkOrderCommand")
	return c.Store.RedispatchWorkOrderCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) RejectPlanningBundle(a0 context.Context, a1 string) (core.PlanningBundle, error) {
	c.count("RejectPlanningBundle")
	return c.Store.RejectPlanningBundle(a0, a1)
}

func (c *taskDetailCountingStore) ReleaseWorkerClaimCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 core.WorkOrderClaimIdentity, a4 core.WorkOrderRelease) (core.WorkOrder, error) {
	c.count("ReleaseWorkerClaimCommand")
	return c.Store.ReleaseWorkerClaimCommand(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) RemoveTaskDependency(a0 context.Context, a1 store.DependencyRemovalRequest) (store.DependencyRemovalResult, error) {
	c.count("RemoveTaskDependency")
	return c.Store.RemoveTaskDependency(a0, a1)
}

func (c *taskDetailCountingStore) RenewWorkerClaimCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 core.WorkOrderClaimIdentity, a4 time.Duration) (core.WorkOrder, error) {
	c.count("RenewWorkerClaimCommand")
	return c.Store.RenewWorkerClaimCommand(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) RepairArtifactMetadata(a0 context.Context, a1 store.ArtifactRepairRequest) (store.ArtifactRepairResult, error) {
	c.count("RepairArtifactMetadata")
	return c.Store.RepairArtifactMetadata(a0, a1)
}

func (c *taskDetailCountingStore) RequestChangesCommand(a0 context.Context, a1 taskops.TaskLease, a2 taskops.RequestChanges) (core.Task, error) {
	c.count("RequestChangesCommand")
	return c.Store.RequestChangesCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) RequestPlanRevisionCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 core.WorkOrderClaimIdentity, a4 string) (store.PlanRevisionRequestResult, error) {
	c.count("RequestPlanRevisionCommand")
	return c.Store.RequestPlanRevisionCommand(a0, a1, a2, a3, a4)
}

func (c *taskDetailCountingStore) RestoreRequirement(a0 context.Context, a1 string, a2 string) error {
	c.count("RestoreRequirement")
	return c.Store.RestoreRequirement(a0, a1, a2)
}

func (c *taskDetailCountingStore) RestoreSystemDesign(a0 context.Context, a1 string, a2 string) error {
	c.count("RestoreSystemDesign")
	return c.Store.RestoreSystemDesign(a0, a1, a2)
}

func (c *taskDetailCountingStore) RetryReviewRoundCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.ReviewRoundRetryRequest, a3 store.ReviewRoundSeatResolution, a4 []core.Job, a5 []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	c.count("RetryReviewRoundCommand")
	return c.Store.RetryReviewRoundCommand(a0, a1, a2, a3, a4, a5)
}

func (c *taskDetailCountingStore) RevokeWorker(a0 context.Context, a1 string) error {
	c.count("RevokeWorker")
	return c.Store.RevokeWorker(a0, a1)
}

func (c *taskDetailCountingStore) SetTaskAssigneeCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 string) (core.Task, error) {
	c.count("SetTaskAssigneeCommand")
	return c.Store.SetTaskAssigneeCommand(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) SkipTaskRefresh(a0 context.Context, a1 string, a2 string, a3 string) error {
	c.count("SkipTaskRefresh")
	return c.Store.SkipTaskRefresh(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) StartOverTaskCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	c.count("StartOverTaskCommand")
	return c.Store.StartOverTaskCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) SubmitImplementationCommand(a0 context.Context, a1 taskops.TaskLease, a2 store.ImplementationSubmission) (core.Job, error) {
	c.count("SubmitImplementationCommand")
	return c.Store.SubmitImplementationCommand(a0, a1, a2)
}

func (c *taskDetailCountingStore) SupersedeReferenceDocument(a0 context.Context, a1 string, a2 core.ReferenceDocumentVersion) (core.ReferenceDocumentVersion, error) {
	c.count("SupersedeReferenceDocument")
	return c.Store.SupersedeReferenceDocument(a0, a1, a2)
}

func (c *taskDetailCountingStore) UpdateGitHubLifecycle(a0 context.Context, a1 core.GitHubLifecycle) error {
	c.count("UpdateGitHubLifecycle")
	return c.Store.UpdateGitHubLifecycle(a0, a1)
}

func (c *taskDetailCountingStore) UpdateJob(a0 context.Context, a1 core.Job) error {
	c.count("UpdateJob")
	return c.Store.UpdateJob(a0, a1)
}

func (c *taskDetailCountingStore) UpdatePullRequestClose(a0 context.Context, a1 core.PullRequestClose) error {
	c.count("UpdatePullRequestClose")
	return c.Store.UpdatePullRequestClose(a0, a1)
}

func (c *taskDetailCountingStore) UpdateReviewPublication(a0 context.Context, a1 core.ReviewPublication) error {
	c.count("UpdateReviewPublication")
	return c.Store.UpdateReviewPublication(a0, a1)
}

func (c *taskDetailCountingStore) UpdateTaskClassification(a0 context.Context, a1 string, a2 string) error {
	c.count("UpdateTaskClassification")
	return c.Store.UpdateTaskClassification(a0, a1, a2)
}

func (c *taskDetailCountingStore) UpdateTaskContext(a0 context.Context, a1 string, a2 store.TaskContextChange) (core.TaskContext, error) {
	c.count("UpdateTaskContext")
	return c.Store.UpdateTaskContext(a0, a1, a2)
}

func (c *taskDetailCountingStore) UpdateWorkOrderCommand(a0 context.Context, a1 taskops.TaskLease, a2 core.WorkOrder, a3 ...core.WorkOrderCommand) error {
	c.count("UpdateWorkOrderCommand")
	return c.Store.UpdateWorkOrderCommand(a0, a1, a2, a3...)
}

func (c *taskDetailCountingStore) UpsertTranscript(a0 context.Context, a1 core.Transcript) error {
	c.count("UpsertTranscript")
	return c.Store.UpsertTranscript(a0, a1)
}

func (c *taskDetailCountingStore) UpsertWorkOrderActivitySnapshot(a0 context.Context, a1 string, a2 core.WorkOrderClaimIdentity, a3 string) error {
	c.count("UpsertWorkOrderActivitySnapshot")
	return c.Store.UpsertWorkOrderActivitySnapshot(a0, a1, a2, a3)
}

func (c *taskDetailCountingStore) ValidateTaskDependencies(a0 context.Context, a1 []string) error {
	c.count("ValidateTaskDependencies")
	return c.Store.ValidateTaskDependencies(a0, a1)
}

func (c *taskDetailCountingStore) WithPlanningSessionFinalization(a0 context.Context, a1 string, a2 func(context.Context) error) error {
	c.count("WithPlanningSessionFinalization")
	return c.Store.WithPlanningSessionFinalization(a0, a1, a2)
}

func (c *taskDetailCountingStore) WithPlanningSessionRun(a0 context.Context, a1 string, a2 func(context.Context) error) error {
	c.count("WithPlanningSessionRun")
	return c.Store.WithPlanningSessionRun(a0, a1, a2)
}

func (c *taskDetailCountingStore) WithTaskSideEffectLock(a0 context.Context, a1 string, a2 func(context.Context) error) error {
	c.count("WithTaskSideEffectLock")
	return c.Store.WithTaskSideEffectLock(a0, a1, a2)
}

func (c *taskDetailCountingStore) WorktreeHandoffCommand(a0 context.Context, a1 taskops.TaskLease, a2 string, a3 core.WorkOrderClaimIdentity, a4 string, a5 core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	c.count("WorktreeHandoffCommand")
	return c.Store.WorktreeHandoffCommand(a0, a1, a2, a3, a4, a5)
}

// countingMemberships counts the membership decisions a request makes.
type countingMemberships struct {
	store.MembershipStore
	mu     sync.Mutex
	single int
	batch  int
}

func (c *countingMemberships) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	c.mu.Lock()
	c.single++
	c.mu.Unlock()
	return c.MembershipStore.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
}

func (c *countingMemberships) AuthorizeWorkspaceCapabilities(ctx context.Context, userID, workspaceID string, capabilities []core.Capability) (map[core.Capability]bool, error) {
	c.mu.Lock()
	c.batch++
	c.mu.Unlock()
	return c.MembershipStore.AuthorizeWorkspaceCapabilities(ctx, userID, workspaceID, capabilities)
}

// legacyTaskActivity is the task-detail assembly as it stood before
// request-scoped reads, kept verbatim as the byte-equality oracle: every
// helper re-reads what it needs, one store call per order and citation.
func legacyTaskActivity(s *Server, w http.ResponseWriter, r *http.Request) {
	bounded := false
	if values, present := r.URL.Query()["view"]; present {
		if len(values) != 1 || values[0] != "bounded" {
			http.Error(w, "view must be bounded or omitted", http.StatusBadRequest)
			return
		}
		bounded = true
	}
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	fail := func(err error) { http.Error(w, "internal server error: "+err.Error(), http.StatusInternalServerError) }
	jobs, err := s.Store.ListJobs(r.Context(), id)
	if err != nil {
		fail(err)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id)
	if err != nil {
		fail(err)
		return
	}
	if task.Context, err = store.TaskContextFromEvents(r.Context(), s.Store, events); err != nil {
		fail(err)
		return
	}
	if task.Context.Proposals, err = s.Store.ListTaskContextProposals(r.Context(), id, core.TaskContextProposalProposed); err != nil {
		fail(err)
		return
	}
	interventions, err := s.Store.ListInterventions(r.Context(), id)
	if err != nil {
		fail(err)
		return
	}
	spec, hasSpec, err := s.Store.GetLatestSpecVersion(r.Context(), id)
	if err != nil {
		fail(err)
		return
	}
	var specPointer *core.SpecVersion
	if hasSpec {
		spec.MaterializedChildren = materializedChildrenForSpec(task.Children, spec.Version)
		specPointer = &spec
	}
	workOrders, err := s.Store.ListTaskWorkOrders(store.WithWorkspace(r.Context(), task.Workspace), id)
	if err != nil {
		fail(err)
		return
	}
	if workOrders == nil {
		workOrders = []core.WorkOrder{}
	}
	_, pendingPlanRevision, planErr := dispatch.PendingPlanRevisionGate(r.Context(), s.Store, id)
	if errors.Is(planErr, store.ErrPlanRevisionGateUnreadable) {
		pendingPlanRevision, planErr = true, nil
	}
	if planErr != nil {
		fail(planErr)
		return
	}
	for index := range workOrders {
		captures, captureErr := s.Store.ListWorkOrderTranscriptCaptures(r.Context(), workOrders[index].ID)
		if captureErr != nil {
			fail(captureErr)
			return
		}
		if len(captures) > 0 {
			workOrders[index].TranscriptCaptures = captures
		}
		if workOrders[index].State == core.WorkOrderClaimed && workOrders[index].AttemptID != "" {
			snapshot, exists, snapshotErr := s.Store.GetWorkOrderActivitySnapshot(r.Context(), workOrders[index].ID)
			if snapshotErr != nil {
				fail(snapshotErr)
				return
			}
			if exists && snapshot.AttemptID == workOrders[index].AttemptID {
				workOrders[index].ActivitySnapshot = &snapshot
			}
		}
		store.ApplyRecoveryRefusal(&workOrders[index], task, workOrders, pendingPlanRevision)
		if workOrders[index].Stage != core.StageImplement {
			continue
		}
		workOrders[index].BlockingTaskIDs = append([]string(nil), task.BlockingTaskIDs...)
		for _, dependency := range task.Dependencies {
			if dependency.State != core.TaskMerged && core.TaskTerminal(dependency.State) {
				workOrders[index].UnsatisfiableTaskIDs = append(workOrders[index].UnsatisfiableTaskIDs, dependency.ID)
			}
		}
		if len(workOrders[index].BlockingTaskIDs) > 0 {
			workOrders[index].Claimable = false
		}
	}
	decorateWorkOrderAgentActivity(workOrders, events)
	checkoutCommand, checkoutAvailable, checkoutGuidance := checkoutStateFromHistory(id, events)
	all, err := s.Store.ListArtifacts(r.Context())
	if err != nil {
		fail(err)
		return
	}
	attachments, verificationEvidence := make([]core.Artifact, 0), make([]core.Artifact, 0)
	for _, artifact := range all {
		if artifact.TaskID != id || artifact.Role != core.ArtifactRoleTaskContext {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		attachments = append(attachments, artifact)
	}
	if all, err = s.Store.ListArtifacts(r.Context()); err != nil {
		fail(err)
		return
	}
	for _, artifact := range all {
		if artifact.TaskID != id || !artifact.EligibleVerificationEvidence() {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		verificationEvidence = append(verificationEvidence, artifact)
	}
	var workerStatus *workerservice.TaskWorkerStatus
	var mergeReadiness *dispatch.MergeReadiness
	gateMerge := task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval
	if (task.State == core.TaskApproved || gateMerge) && s.OnMergeReadiness != nil {
		var readiness dispatch.MergeReadiness
		var readinessErr error
		if gateMerge {
			readiness, readinessErr = s.taskRunReadiness.read(r.Context(), task, s.OnMergeReadiness)
		} else {
			readiness, readinessErr = s.OnMergeReadiness(r.Context(), task)
		}
		if readinessErr != nil {
			http.Error(w, fmt.Sprintf("resolve merge readiness: %v", readinessErr), http.StatusServiceUnavailable)
			return
		}
		mergeReadiness = &readiness
		if refreshed, refreshErr := s.Store.GetTask(r.Context(), id); refreshErr == nil {
			task = refreshed
		}
	}
	if s.Workers != nil && s.ConfigProvider != nil && !core.TaskTerminal(task.State) {
		if cfg, cfgErr := s.ConfigProvider(r.Context()); cfgErr == nil {
			workerStatus = s.Workers.TaskAvailability(r.Context(), cfg, task, workOrders)
		}
	}
	stalled := store.StalledTask(workOrders)
	if core.TaskTerminal(task.State) {
		stalled = nil
	}
	proposals, err := s.Store.ListPendingProposals(r.Context())
	if err != nil {
		fail(err)
		return
	}
	workOrderViews, err := legacyCheckpointWorkOrderViews(s, r.Context(), workOrders, proposals)
	if err != nil {
		fail(err)
		return
	}
	operatorNotes, err := store.OperatorNotesForTask(r.Context(), s.Store, id)
	if err != nil {
		fail(err)
		return
	}
	pendingAuthority := pendingAuthorityForTask(id, workOrders, proposals)
	claimWaiting, err := s.proposalClaimWaitingByTask(r.Context(), workOrders)
	if err != nil {
		fail(err)
		return
	}
	waitingProposals := claimWaiting[id]
	opts, err := s.nextActionOptions(r)
	if err != nil {
		fail(err)
		return
	}
	action, err := store.ReadNextAction(r.Context(), s.Store, id, opts)
	if err != nil {
		fail(err)
		return
	}
	compact := nextaction.CompactFrom(action)
	item := reviewItem{
		Task: task, Jobs: jobs, Events: events, Interventions: interventions,
		CheckoutCommand: checkoutCommand, CheckoutAvailable: checkoutAvailable, CheckoutGuidance: checkoutGuidance,
		NeedsAttention:            action.Actor == nextaction.ActorOperator,
		NextAction:                &compact,
		AtMergeGate:               store.AtMergeGate(task, events),
		PendingAuthority:          pendingAuthority,
		ProposalClaimWaiting:      len(waitingProposals) > 0,
		WaitingProposals:          waitingProposals,
		ForgeFailure:              store.LatestForgeFailure(events),
		Spec:                      specPointer,
		WorkOrders:                workOrderViews,
		ReviewDiagnostics:         store.ReviewVerdictDiagnostics(workOrders, events, time.Now().UTC()),
		ReviewRecovery:            store.ReviewRecoveryNeeded(workOrders, events),
		InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(workOrders, events), events),
		Stalled:                   stalled,
		WorkerStatus:              workerStatus,
		MergeReadiness:            mergeReadiness,
		Attachments:               attachments,
		VerificationEvidence:      verificationEvidence,
		OperatorNotes:             operatorNotes,
	}
	if !bounded {
		writeJSON(w, http.StatusOK, taskDetailProjection{item})
		return
	}
	pageEvents, eventPage, err := s.boundedTaskEventSection(r, id, events)
	if err != nil {
		fail(err)
		return
	}
	summaries := deriveTaskDetailSummaries(events, jobs, workOrders)
	if err := s.enrichReviewSeatSummaries(r.Context(), task, workOrders, events, &summaries); err != nil {
		fail(err)
		return
	}
	item.Events = nil
	w.Header().Set("X-Conveyor-Detail-View", "bounded")
	writeJSON(w, http.StatusOK, boundedTaskDetailProjection{reviewItem: item, events: pageEvents, eventPage: eventPage, summaries: summaries})
}

// legacyCheckpointWorkOrderViews is the per-citation enrichment the batched
// read replaced: GetRequirement, then GetSystemDesign, for every citation.
func legacyCheckpointWorkOrderViews(s *Server, ctx context.Context, orders []core.WorkOrder, proposals []core.PendingProposal) ([]workOrderActivityView, error) {
	views := make([]workOrderActivityView, 0, len(orders))
	for _, order := range orders {
		view := workOrderActivityView{WorkOrder: order}
		if order.Checkpoint == nil {
			views = append(views, view)
			continue
		}
		checkpoint := &checkpointActivityView{DecisionRequest: order.Checkpoint.DecisionRequest, Class: order.Checkpoint.Class, Verification: order.Checkpoint.Verification}
		if len(order.Checkpoint.Citations) > 0 {
			checkpoint.Citations = make([]checkpointCitationActivityView, 0, len(order.Checkpoint.Citations))
		}
		for _, citation := range order.Checkpoint.Citations {
			enriched := checkpointCitationActivityView{DocumentID: citation.DocumentID, CitedVersion: citation.CitedVersion, StatementOrSectionID: citation.StatementOrSectionID, PendingProposals: []core.PendingProposal{}}
			if requirement, err := s.Store.GetRequirement(ctx, citation.DocumentID); err == nil {
				enriched.DocumentKind, enriched.DocumentTitle, enriched.CurrentConfirmedVersion = "requirement", requirement.Title, requirement.CurrentVersion
			} else if document, designErr := s.Store.GetSystemDesign(ctx, citation.DocumentID); designErr == nil {
				enriched.DocumentKind, enriched.DocumentTitle, enriched.CurrentConfirmedVersion = "system_design", document.Title, document.CurrentVersion
			} else {
				return nil, fmt.Errorf("enrich checkpoint citation %s: requirement lookup: %v; system design lookup: %v", citation.DocumentID, err, designErr)
			}
			enriched.NewerConfirmed = enriched.CurrentConfirmedVersion > enriched.CitedVersion
			for _, proposal := range proposals {
				if proposal.ID == citation.DocumentID && proposal.Tier == enriched.DocumentKind {
					enriched.PendingProposals = append(enriched.PendingProposals, proposal)
				}
			}
			checkpoint.Citations = append(checkpoint.Citations, enriched)
		}
		view.Checkpoint = checkpoint
		views = append(views, view)
	}
	return views, nil
}

// detailFixture is one workspace whose task-detail branches each grow work
// orders, attempts with captures and snapshots, distinct, repeated, and
// colliding citations, active, removed, and re-pinned context, attachments,
// evidence, and event history by size.
type detailFixture struct {
	*batchHTTPFixture
	size      int
	branches  map[string]string
	artifacts int
}

var detailBranches = []string{"running", "checkpoint", "plan", "gate", "approved", "merged"}

func newDetailFixture(t *testing.T, size int) *detailFixture {
	t.Helper()
	f := &detailFixture{batchHTTPFixture: newBatchHTTPFixture(t), size: size, branches: map[string]string{}}
	other := f.task(t, "unrelated", core.TaskRunning, "")
	for i := 0; i < size; i++ {
		f.artifact(t, other.ID, core.ArtifactRoleTaskContext, fmt.Sprintf("other-%d.txt", i), "text/plain", []byte(fmt.Sprintf("other %d %s", i, other.ID)))
	}
	f.branches["running"] = f.grow(t, f.task(t, "running", core.TaskRunning, ""), true).ID
	f.branches["checkpoint"] = f.grow(t, f.task(t, "checkpoint", core.TaskRunning, ""), false, true).ID
	plan := f.task(t, "plan", core.TaskAwaiting, core.GateKindPlanApproval)
	if _, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: plan.ID, Content: "## Approach\n\nPlan.", CreatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	f.branches["plan"] = f.grow(t, plan, false).ID
	gate := f.task(t, "gate", core.TaskAwaiting, core.GateKindMergeApproval)
	f.branches["gate"] = f.grow(t, gate, false).ID
	approved := f.task(t, "approved", core.TaskApproved, "")
	f.branches["approved"] = f.grow(t, approved, false).ID
	merged := f.task(t, "merged", core.TaskMerged, "")
	f.branches["merged"] = f.grow(t, merged, false).ID
	return f
}

func (f *detailFixture) artifact(t *testing.T, taskID string, role core.ArtifactRole, name, contentType string, content []byte) {
	t.Helper()
	// Distinct times keep the legacy oracle's order deterministic: its
	// volatile sort compared creation times alone.
	f.artifacts++
	if _, err := f.st.CreateArtifact(f.ctx, core.Artifact{Workspace: "demo", Name: name, ContentType: contentType, Role: role, TaskID: taskID, CreatedAt: f.now.Add(time.Duration(f.artifacts) * time.Millisecond)}, content); err != nil {
		t.Fatalf("create artifact %s: %v", name, err)
	}
}

func (f *detailFixture) design(t *testing.T, id string) string {
	t.Helper()
	design, version, err := f.st.CreateSystemDesign(f.ctx, core.SystemDesign{ID: id, Title: "Design " + id, Category: "Component design"}, core.SystemDesignVersion{Content: "# Design " + id + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/" + id + "/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.st.ConfirmSystemDesignVersion(f.ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	return design.ID
}

// grow adds size-scaled shapes to one task. claimed adds attempts with
// captures and snapshots, which only a running task can hold.
func (f *detailFixture) grow(t *testing.T, task core.Task, claimed bool, checkpointLast ...bool) core.Task {
	t.Helper()
	var requirements, designs []string
	var citations []core.WorkOrderAuthorityConflictCitation
	for i := 0; i < f.size; i++ {
		requirement := f.requirement(t, fmt.Sprintf("context %d", i), true)
		design := f.design(t, fmt.Sprintf("dsn-%d-%s", i, strings.ToLower(core.NewTaskID())))
		requirements, designs = append(requirements, requirement), append(designs, design)
		citations = append(citations,
			core.WorkOrderAuthorityConflictCitation{DocumentID: requirement, CitedVersion: 1, StatementOrSectionID: "REQ-1"},
			core.WorkOrderAuthorityConflictCitation{DocumentID: design, CitedVersion: 1},
			core.WorkOrderAuthorityConflictCitation{DocumentID: requirement, CitedVersion: 1, StatementOrSectionID: "REQ-1"})
		f.artifact(t, task.ID, core.ArtifactRoleTaskContext, fmt.Sprintf("context-%d.txt", i), "text/plain", []byte(fmt.Sprintf("attachment %d %s", i, task.ID)))
		f.artifact(t, task.ID, core.ArtifactRoleVerificationEvidence, fmt.Sprintf("evidence-%d.png", i), "image/png", detailPNG(t, task.ID, i))
	}
	// A terminal task refuses context changes; its history keeps none.
	if !core.TaskTerminal(task.State) {
		if _, err := f.st.UpdateTaskContext(f.ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: requirements, DesignIDs: designs}}); err != nil {
			t.Fatalf("attach context: %v", err)
		}
		if _, err := f.st.UpdateTaskContext(f.ctx, task.ID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: requirements[:1]}}); err != nil {
			t.Fatalf("remove context: %v", err)
		}
		if err := f.st.ArchiveRequirement(f.ctx, requirements[len(requirements)-1], "operator", nil); err != nil {
			t.Fatalf("archive requirement: %v", err)
		}
	}
	// The checkpoint order is first, or last so its release is the task's
	// current agent question.
	checkpointIndex := 0
	if len(checkpointLast) > 0 && checkpointLast[0] {
		checkpointIndex = f.size
	}
	for i := 0; i <= f.size; i++ {
		id := fmt.Sprintf("%s-implement-%d", task.ID, i+1)
		if err := f.st.CreateJob(f.ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: f.now.Add(time.Duration(i) * time.Second), QueueDeadline: f.now.Add(time.Hour), CreatedAt: f.now.Add(time.Duration(i) * time.Second)}
		if i == checkpointIndex {
			order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
			order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited documents.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: citations}
		}
		if err := storetest.CreateWorkOrder(f.ctx, f.st, order); err != nil {
			t.Fatal(err)
		}
		if claimed && i > 0 {
			f.attempts(t, id, i == f.size)
		}
		for e := 0; e < 4; e++ {
			if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: task.ID, JobID: id, Kind: "work_order.progress_reported", ActorRole: core.ActorAgent, At: f.now.Add(time.Duration(i*10+e) * time.Second), Payload: core.JSONPayload(map[string]any{"message": fmt.Sprintf("progress %d of %d", e, i)})}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return task
}

// attempts ends one attempt with a capture whose snapshot stays behind; live
// opens a second attempt whose snapshot matches, as only one order of a task
// can be claimed.
func (f *detailFixture) attempts(t *testing.T, orderID string, live bool) {
	t.Helper()
	const worker = "detail-worker"
	first, err := storetest.ClaimWorkOrder(f.ctx, f.st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: "first-" + orderID, ClientToken: "first-" + orderID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatalf("claim %s: %v", orderID, err)
	}
	identity := core.WorkOrderClaimIdentity{WorkerID: worker, ClaimantID: worker, SessionID: first.SessionID}
	if err = f.st.UpsertWorkOrderActivitySnapshot(f.ctx, orderID, identity, "first tail "+orderID); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ReleaseWorkerClaim(f.ctx, f.st, orderID, worker, core.WorkOrderRelease{SessionID: first.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond}); err != nil {
		t.Fatalf("release %s: %v", orderID, err)
	}
	if _, err = f.st.RecordWorkOrderAttemptCapture(f.ctx, orderID, identity, core.WorkOrderAttemptCapture{SessionID: first.SessionID, AttemptID: first.AttemptID, Transcript: &core.WorkOrderAttemptTranscript{Content: "transcript " + orderID, Truncated: true}}); err != nil {
		t.Fatalf("capture %s: %v", orderID, err)
	}
	if !live {
		return
	}
	second, err := storetest.ClaimWorkOrder(f.ctx, f.st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: "second-" + orderID, ClientToken: "second-" + orderID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatalf("reclaim %s: %v", orderID, err)
	}
	if err = f.st.UpsertWorkOrderActivitySnapshot(f.ctx, orderID, core.WorkOrderClaimIdentity{WorkerID: worker, ClaimantID: worker, SessionID: second.SessionID}, "second tail "+orderID); err != nil {
		t.Fatal(err)
	}
}

// detailServer serves the fixture through a counting store, a fake readiness
// clock, and a counted, non-mutating readiness hook.
type detailServer struct {
	server  *Server
	counter *taskDetailCountingStore
	clock   time.Time
	hooks   int
}

func newDetailServer(f *detailFixture) *detailServer {
	d := &detailServer{counter: newTaskDetailCountingStore(f.st), clock: f.now}
	d.server = NewServer(d.counter)
	d.server.BearerToken, d.server.Workspace = "operator-token", "demo"
	d.server.taskRunReadiness.clock = func() time.Time { return d.clock }
	d.server.OnMergeReadiness = func(_ context.Context, task core.Task) (dispatch.MergeReadiness, error) {
		d.hooks++
		return dispatch.MergeReadiness{State: "MERGEABLE", HeadSHA: "head-" + task.ID, URL: "https://github.com/acme/conveyor/pull/7", Number: 7}, nil
	}
	return d
}

func (d *detailServer) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	d.server.Handler().ServeHTTP(response, request)
	return response
}

func detailPath(id string, bounded bool) string {
	if bounded {
		return "/v1/tasks/" + id + "/activity?workspace_id=demo&view=bounded"
	}
	return "/v1/tasks/" + id + "/activity?workspace_id=demo"
}

// detailForbidden are the per-item and workspace-wide reads the detail no
// longer issues.
var detailForbidden = []string{"GetRequirement", "GetSystemDesign", "ListArtifacts", "ListWorkOrderTranscriptCaptures", "GetWorkOrderActivitySnapshot", "ReadVerificationReview"}

// detailCallVectors are the exact per-method store-call vectors of each
// branch through the real route, cold (no cached readiness) and warm. They
// hold for every fixture size; route middleware calls are included.
var detailCallVectors = map[string]map[string]int{
	"running bounded=false cold":    {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskActivitySnapshots": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"running bounded=false warm":    {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskActivitySnapshots": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"running bounded=true cold":     {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskActivitySnapshots": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"running bounded=true warm":     {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskActivitySnapshots": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"checkpoint bounded=false cold": {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentHeads": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextDesignPins": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"checkpoint bounded=false warm": {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentHeads": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextDesignPins": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"checkpoint bounded=true cold":  {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentHeads": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextDesignPins": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"checkpoint bounded=true warm":  {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentHeads": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextDesignPins": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"plan bounded=false cold":       {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"plan bounded=false warm":       {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"plan bounded=true cold":        {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"plan bounded=true warm":        {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"gate bounded=false cold":       {"GetLatestSpecVersion": 1, "GetTask": 2, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"gate bounded=false warm":       {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"gate bounded=true cold":        {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 2, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"gate bounded=true warm":        {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListLatestSpecVersionNumbers": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"approved bounded=false cold":   {"GetLatestSpecVersion": 1, "GetTask": 2, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"approved bounded=false warm":   {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"approved bounded=true cold":    {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 2, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"approved bounded=true warm":    {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListDocumentOperatorNotesForTask": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListNextActionHistory": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 2, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"merged bounded=false cold":     {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 1, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"merged bounded=false warm":     {"GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListTaskContextProposals": 1, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1},
	"merged bounded=true cold":      {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 1, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
	"merged bounded=true warm":      {"GetApprovedSpecVersion": 1, "GetLatestSpecVersion": 1, "GetTask": 1, "ListActivityMarkersForTasks": 1, "ListArtifactsForLineage": 1, "ListDependencyBlockers": 1, "ListDocumentMetadata": 1, "ListEvents": 1, "ListInterventions": 1, "ListJobs": 1, "ListOpenDependents": 1, "ListPendingProposals": 2, "ListReviewRoundSeats": 1, "ListTaskContextProposals": 1, "ListTaskTranscriptCaptures": 1, "ListTaskWorkOrders": 1, "ReadTaskEventWindow": 1},
}

func TestTaskDetailStoreCallsStayFixed(t *testing.T) {
	vectors := map[string]map[string]int{}
	for _, size := range []int{1, 5} {
		f := newDetailFixture(t, size)
		d := newDetailServer(f)
		for _, branch := range detailBranches {
			for _, bounded := range []bool{false, true} {
				for _, warmth := range []string{"cold", "warm"} {
					id := f.branches[branch]
					if warmth == "cold" {
						d.server.taskRunReadiness.forget(store.WithWorkspace(context.Background(), "demo"), id)
					} else {
						// The warm entry predates the request's task read.
						d.clock = d.clock.Add(time.Second)
					}
					d.counter.reset()
					hooks := d.hooks
					response := d.get(t, detailPath(id, bounded))
					if response.Code != http.StatusOK {
						t.Fatalf("size %d %s bounded=%v %s: status %d %s", size, branch, bounded, warmth, response.Code, response.Body)
					}
					calls := d.counter.reset()
					label := fmt.Sprintf("%s bounded=%v %s", branch, bounded, warmth)
					readiness := branch == "gate" || branch == "approved"
					wantTask, wantHooks := 1, 0
					if readiness && warmth == "cold" {
						wantTask, wantHooks = 2, 1
					}
					if calls["GetTask"] != wantTask || d.hooks-hooks != wantHooks {
						t.Fatalf("size %d %s: GetTask=%d hooks=%d, want %d and %d", size, label, calls["GetTask"], d.hooks-hooks, wantTask, wantHooks)
					}
					if calls["ListEvents"] != 1 || calls["ListTaskWorkOrders"] != 1 || calls["ListArtifactsForLineage"] != 1 {
						t.Fatalf("size %d %s: full reads %v", size, label, calls)
					}
					for _, name := range []string{"ListTaskTranscriptCaptures", "ListTaskActivitySnapshots", "ListDocumentMetadata"} {
						if calls[name] > 1 {
							t.Fatalf("size %d %s: %s called %d times", size, label, name, calls[name])
						}
					}
					for _, name := range detailForbidden {
						if calls[name] != 0 {
							t.Fatalf("size %d %s: forbidden %s called %d times", size, label, name, calls[name])
						}
					}
					if previous, seen := vectors[label]; seen && !reflect.DeepEqual(previous, calls) {
						t.Fatalf("%s: call vector grew with size:\nsize 1: %v\nsize %d: %v", label, previous, size, calls)
					}
					vectors[label] = calls
				}
			}
		}
	}
	for label, calls := range vectors {
		if want, ok := detailCallVectors[label]; ok && !reflect.DeepEqual(want, calls) {
			t.Fatalf("%s: vector %v, want %v", label, calls, want)
		}
		if _, ok := detailCallVectors[label]; !ok {
			t.Errorf("%s: no recorded vector; got %#v", label, calls)
		}
	}
}

// detailPNG encodes a distinct valid PNG per task and index, so each
// evidence artifact has its own content address.
func detailPNG(t *testing.T, taskID string, index int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2+index, 1+len(taskID)%7))
	for i, b := range []byte(taskID) {
		img.Set(i%img.Bounds().Dx(), 0, color.RGBA{R: b, G: byte(index), B: byte(i), A: 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// detailRequest builds the same authenticated, routed request context the
// middleware builds for the operator bearer token, so the new handler and the
// legacy oracle read exactly the same inputs.
func detailRequest(t *testing.T, id string, bounded bool) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, detailPath(id, bounded), nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", id)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: "local-operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	ctx = store.WithWorkspace(store.WithCredential(store.WithActor(ctx, store.Actor{ID: "user:local-operator", Role: core.ActorUser}), credential), "demo")
	return request.WithContext(ctx)
}

func serveDetail(t *testing.T, handler func(http.ResponseWriter, *http.Request), id string, bounded bool) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler(response, detailRequest(t, id, bounded))
	return response
}

// withoutTaskContext removes task.context, which the legacy assembly lost
// for approved and merge-gate tasks: its post-readiness reload replaced the
// task after the context was computed.
func withoutTaskContext(t *testing.T, body []byte) ([]byte, json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(fields["task"], &task); err != nil {
		t.Fatalf("task field: %v in %s", err, body)
	}
	context := task["context"]
	delete(task, "context")
	fields["task"], _ = json.Marshal(task)
	out, _ := json.Marshal(fields)
	return out, context
}

func TestTaskDetailBodiesMatchPreviousAssembly(t *testing.T) {
	for _, size := range []int{1, 4} {
		f := newDetailFixture(t, size)
		d := newDetailServer(f)
		for _, branch := range detailBranches {
			for _, bounded := range []bool{false, true} {
				id := f.branches[branch]
				label := fmt.Sprintf("size %d %s bounded=%v", size, branch, bounded)
				legacy := serveDetail(t, func(w http.ResponseWriter, r *http.Request) { legacyTaskActivity(d.server, w, r) }, id, bounded)
				current := serveDetail(t, d.server.getTaskActivity, id, bounded)
				if legacy.Code != http.StatusOK || current.Code != http.StatusOK {
					t.Fatalf("%s: legacy %d %s, current %d %s", label, legacy.Code, legacy.Body, current.Code, current.Body)
				}
				if legacy.Header().Get("X-Conveyor-Detail-View") != current.Header().Get("X-Conveyor-Detail-View") {
					t.Fatalf("%s: view headers differ", label)
				}
				if branch != "gate" && branch != "approved" {
					if !bytes.Equal(legacy.Body.Bytes(), current.Body.Bytes()) {
						t.Fatalf("%s: body differs\nlegacy:  %s\ncurrent: %s", label, legacy.Body, current.Body)
					}
					continue
				}
				legacyRest, legacyContext := withoutTaskContext(t, legacy.Body.Bytes())
				currentRest, currentContext := withoutTaskContext(t, current.Body.Bytes())
				if !bytes.Equal(legacyRest, currentRest) {
					t.Fatalf("%s: body outside task.context differs\nlegacy:  %s\ncurrent: %s", label, legacyRest, currentRest)
				}
				events, err := f.st.ListEvents(f.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				want, err := store.TaskContextFromEvents(f.ctx, f.st, events)
				if err != nil {
					t.Fatal(err)
				}
				if want.Proposals, err = f.st.ListTaskContextProposals(f.ctx, id, core.TaskContextProposalProposed); err != nil {
					t.Fatal(err)
				}
				wantJSON, _ := json.Marshal(want)
				if string(currentContext) != string(wantJSON) || string(legacyContext) != "{}" {
					t.Fatalf("%s: context current=%s want=%s legacy=%s", label, currentContext, wantJSON, legacyContext)
				}
			}
		}
	}
}

func TestTaskDetailCapabilitiesMatchPerCapabilityAuthority(t *testing.T) {
	roles := []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator, ""}
	credentials := map[string]core.AuthenticatedCredential{
		"user operator scope": {ID: "pat", OwnerUserID: "usr_member", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"user plain scope":    {ID: "pat", OwnerUserID: "usr_member", Kind: core.CredentialUser},
		"agent":               {ID: "agent", OwnerUserID: "usr_member", Kind: core.CredentialAgent, Scope: core.CredentialScopeOperator},
		// A worker reaches reads through its actor, without a user credential.
		"worker": {},
	}
	for _, role := range roles {
		for name, credential := range credentials {
			for _, scoped := range []bool{true, false} {
				for _, configured := range []bool{true, false} {
					label := fmt.Sprintf("role=%q %s scoped=%v memberships=%v", role, name, scoped, configured)
					members := &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{}}
					if role != "" {
						members.roles["usr_member"] = map[string]core.WorkspaceRole{"demo": role}
					}
					counted := &countingMemberships{MembershipStore: members}
					server := NewServer(store.NewMemory())
					if configured {
						server.Memberships = counted
					}
					ctx := store.WithActor(t.Context(), store.Actor{ID: store.WorkerActorID("detail-worker"), Role: core.ActorWorker})
					if credential.Kind != "" {
						ctx = store.WithCredential(ctx, credential)
					}
					if scoped {
						ctx = store.WithWorkspace(ctx, "demo")
					}
					want, wantErr := server.nextActionOptionsFor(ctx)
					single := counted.single
					counted.single = 0
					got, gotErr := server.detailNextActionOptions(ctx)
					if wantErr != nil || gotErr != nil || !reflect.DeepEqual(want.Capabilities, got.Capabilities) {
						t.Fatalf("%s: batched %v (%v), per capability %v (%v)", label, got.Capabilities, gotErr, want.Capabilities, wantErr)
					}
					if got.MergeReadiness == nil || counted.single != 0 || counted.batch > 1 || (counted.batch == 1) != (single > 0) {
						t.Fatalf("%s: batch=%d single=%d (per-capability path made %d)", label, counted.batch, counted.single, single)
					}
				}
			}
		}
	}
	// A revoked binding grants nothing, and an authority failure fails closed.
	members := &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{"usr_member": {"demo": core.WorkspaceRoleOperator}}}
	server := NewServer(store.NewMemory())
	server.Memberships = members
	ctx := store.WithWorkspace(store.WithCredential(t.Context(), credentials["user plain scope"]), "demo")
	if granted, err := server.detailNextActionOptions(ctx); err != nil || len(granted.Capabilities) != len(nextActionCapabilities) {
		t.Fatalf("operator binding: %v %v", granted.Capabilities, err)
	}
	delete(members.roles, "usr_member")
	if revoked, err := server.detailNextActionOptions(ctx); err != nil || len(revoked.Capabilities) != 0 {
		t.Fatalf("revoked binding: %v %v", revoked.Capabilities, err)
	}
	members.authorizeErrs = map[core.Capability]error{core.CapabilityRecoverWork: errors.New("authority unavailable")}
	if _, err := server.detailNextActionOptions(ctx); err == nil {
		t.Fatal("authority failure granted capabilities")
	}

	// Through the route, an authority failure answers 500 and the batch
	// replaces the four per-capability decisions.
	f := newDetailFixture(t, 1)
	d := newDetailServer(f)
	counted := &countingMemberships{MembershipStore: &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{"local-operator": {"demo": core.WorkspaceRoleOperator}}}}
	d.server.Memberships = counted
	if response := d.get(t, detailPath(f.branches["checkpoint"], true)); response.Code != http.StatusOK || counted.batch != 1 {
		t.Fatalf("routed read: %d batch=%d single=%d %s", response.Code, counted.batch, counted.single, response.Body)
	}
	counted.MembershipStore.(*membershipFixture).authorizeErrs = map[core.Capability]error{core.CapabilityConfirmDocuments: errors.New("authority unavailable")}
	// Route admission is a separate decision; fail only the detail batch.
	d.server.Memberships = &batchFailingMemberships{MembershipStore: counted}
	if response := d.get(t, detailPath(f.branches["checkpoint"], true)); response.Code != http.StatusInternalServerError {
		t.Fatalf("authority failure: %d %s", response.Code, response.Body)
	}
}

// batchFailingMemberships admits routes but fails the detail's batch.
type batchFailingMemberships struct{ store.MembershipStore }

func (b *batchFailingMemberships) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	return true, nil
}

func (b *batchFailingMemberships) AuthorizeWorkspaceCapabilities(context.Context, string, string, []core.Capability) (map[core.Capability]bool, error) {
	return nil, errors.New("authority unavailable")
}

// metadataFailingStore fails the detail's one document-metadata set read.
type metadataFailingStore struct{ store.Store }

func (m metadataFailingStore) ListDocumentMetadata(context.Context, []string) (store.DocumentMetadataSet, error) {
	return store.DocumentMetadataSet{}, errors.New("metadata unavailable")
}

func TestTaskDetailDocumentAndArtifactSelections(t *testing.T) {
	f := &detailFixture{batchHTTPFixture: newBatchHTTPFixture(t), branches: map[string]string{}}
	task := f.task(t, "selections", core.TaskRunning, "")
	// A requirement and a System Design share one ID: the citation resolves
	// to the requirement, the design attachment to the design.
	shared := f.requirement(t, "shared", true)
	f.design(t, shared)
	design := f.design(t, "dsn-selection-"+strings.ToLower(core.NewTaskID()))
	if _, err := f.st.UpdateTaskContext(f.ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{shared}, DesignIDs: []string{shared, design}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ArchiveSystemDesign(f.ctx, design, "operator", nil); err != nil {
		t.Fatal(err)
	}
	f.order(t, task, core.StageImplement, func(order *core.WorkOrder) {
		order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
		order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Decide the shared document.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: shared, CitedVersion: 1}, {DocumentID: design, CitedVersion: 1}, {DocumentID: shared, CitedVersion: 1}}}
	})
	// One content linked to the task twice, as an attachment and as
	// evidence, beside artifacts that tie on creation time.
	evidence := detailPNG(t, task.ID, 9)
	for _, role := range []core.ArtifactRole{core.ArtifactRoleTaskContext, core.ArtifactRoleVerificationEvidence} {
		if _, err := f.st.CreateArtifact(f.ctx, core.Artifact{Workspace: "demo", Name: "shared.png", ContentType: "image/png", Role: role, TaskID: task.ID, CreatedAt: f.now}, evidence); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if _, err := f.st.CreateArtifact(f.ctx, core.Artifact{Workspace: "demo", Name: fmt.Sprintf("tie-%d.txt", i), ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: task.ID, CreatedAt: f.now}, []byte(fmt.Sprintf("tie %d %s", i, task.ID))); err != nil {
			t.Fatal(err)
		}
	}
	d := newDetailServer(f)
	legacy := serveDetail(t, func(w http.ResponseWriter, r *http.Request) { legacyTaskActivity(d.server, w, r) }, task.ID, false)
	current := serveDetail(t, d.server.getTaskActivity, task.ID, false)
	if legacy.Code != http.StatusOK || current.Code != http.StatusOK {
		t.Fatalf("legacy %d, current %d %s", legacy.Code, current.Code, current.Body)
	}
	var legacyBody, currentBody struct {
		Task                 core.Task              `json:"task"`
		WorkOrders           []json.RawMessage      `json:"work_orders"`
		Attachments          []core.Artifact        `json:"attachments"`
		VerificationEvidence []core.Artifact        `json:"verification_evidence"`
		Rest                 map[string]interface{} `json:"-"`
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.Body.Bytes(), &currentBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacyBody.Task.Context, currentBody.Task.Context) || !reflect.DeepEqual(legacyBody.WorkOrders, currentBody.WorkOrders) {
		t.Fatalf("context or checkpoint differs:\nlegacy:  %+v %s\ncurrent: %+v %s", legacyBody.Task.Context, legacyBody.WorkOrders, currentBody.Task.Context, currentBody.WorkOrders)
	}
	if len(currentBody.Task.Context.Designs) != 2 || currentBody.Task.Context.Designs[0].Title == "" || !strings.Contains(string(currentBody.WorkOrders[0]), `"document_kind":"requirement"`) {
		t.Fatalf("shared ID lost a tier: %+v %s", currentBody.Task.Context, currentBody.WorkOrders[0])
	}
	// Ties keep the legacy membership, in the deterministic (created_at, id,
	// role) order of the task-node read.
	for name, pair := range map[string][2][]core.Artifact{"attachments": {legacyBody.Attachments, currentBody.Attachments}, "evidence": {legacyBody.VerificationEvidence, currentBody.VerificationEvidence}} {
		was, now := append([]core.Artifact(nil), pair[0]...), pair[1]
		sort.Slice(was, func(i, j int) bool {
			if !was[i].CreatedAt.Equal(was[j].CreatedAt) {
				return was[i].CreatedAt.Before(was[j].CreatedAt)
			}
			if was[i].ID != was[j].ID {
				return was[i].ID < was[j].ID
			}
			return was[i].Role < was[j].Role
		})
		if !reflect.DeepEqual(was, now) {
			t.Fatalf("%s: legacy (sorted) %+v, current %+v", name, was, now)
		}
	}
	if len(currentBody.Attachments) != 5 || len(currentBody.VerificationEvidence) != 1 || currentBody.VerificationEvidence[0].DownloadURL != "/v1/artifacts/"+currentBody.VerificationEvidence[0].ID {
		t.Fatalf("attachments=%+v evidence=%+v", currentBody.Attachments, currentBody.VerificationEvidence)
	}

	// A citation with neither row fails both assemblies.
	missing := f.task(t, "missing citation", core.TaskRunning, "")
	f.order(t, missing, core.StageImplement, func(order *core.WorkOrder) {
		order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
		order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Decide.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: "missing-document", CitedVersion: 1}}}
	})
	if legacy, current := serveDetail(t, func(w http.ResponseWriter, r *http.Request) { legacyTaskActivity(d.server, w, r) }, missing.ID, false), serveDetail(t, d.server.getTaskActivity, missing.ID, false); legacy.Code != http.StatusInternalServerError || current.Code != http.StatusInternalServerError {
		t.Fatalf("missing citation: legacy %d, current %d", legacy.Code, current.Code)
	}
	// A failed metadata set read fails closed.
	failing := NewServer(metadataFailingStore{f.st})
	if response := serveDetail(t, failing.getTaskActivity, task.ID, true); response.Code != http.StatusInternalServerError {
		t.Fatalf("metadata failure: %d %s", response.Code, response.Body)
	}
}

// gatedHook is a readiness hook whose calls block until released, so tests
// order concurrent readers with channels instead of sleeps.
type gatedHook struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	result  func(context.Context, core.Task) (dispatch.MergeReadiness, error)
}

func newGatedHook(result func(context.Context, core.Task) (dispatch.MergeReadiness, error)) *gatedHook {
	return &gatedHook{entered: make(chan struct{}, 16), release: make(chan struct{}), result: result}
}

func (g *gatedHook) call(ctx context.Context, task core.Task) (dispatch.MergeReadiness, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return dispatch.MergeReadiness{}, ctx.Err()
	}
	return g.result(ctx, task)
}

func (g *gatedHook) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func mergeable(_ context.Context, task core.Task) (dispatch.MergeReadiness, error) {
	return dispatch.MergeReadiness{State: "MERGEABLE", HeadSHA: task.ApprovedHeadSHA}, nil
}

func TestTaskDetailReadinessCache(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "task-a", State: core.TaskApproved, ApprovedHeadSHA: "head-1"}
	immediate := func(result func(context.Context, core.Task) (dispatch.MergeReadiness, error)) (*int, func(context.Context, core.Task) (dispatch.MergeReadiness, error)) {
		calls := 0
		return &calls, func(ctx context.Context, task core.Task) (dispatch.MergeReadiness, error) {
			calls++
			return result(ctx, task)
		}
	}

	t.Run("ConcurrentMissesShareOneCall", func(t *testing.T) {
		cache := &taskRunReadinessCache{clock: func() time.Time { return now }}
		hook := newGatedHook(mergeable)
		results := make(chan bool, 4)
		go func() {
			_, fresh, err := cache.readObserved(ctx, task, hook.call, now)
			results <- err == nil && fresh
		}()
		<-hook.entered
		for i := 0; i < 3; i++ {
			go func() {
				_, fresh, err := cache.readObserved(ctx, task, hook.call, now)
				results <- err == nil && fresh
			}()
		}
		close(hook.release)
		for i := 0; i < 4; i++ {
			if !<-results {
				t.Fatal("a reader of the shared call failed or was not fresh")
			}
		}
		if hook.count() != 1 {
			t.Fatalf("hook calls = %d, want 1", hook.count())
		}
	})

	t.Run("ExpiryKeysAndFailures", func(t *testing.T) {
		clock := now
		cache := &taskRunReadinessCache{clock: func() time.Time { return clock }}
		calls, hook := immediate(mergeable)
		read := func(task core.Task, since time.Time) bool {
			t.Helper()
			_, fresh, err := cache.readObserved(ctx, task, hook, since)
			if err != nil {
				t.Fatal(err)
			}
			return fresh
		}
		if !read(task, clock) || *calls != 1 {
			t.Fatalf("first read calls=%d", *calls)
		}
		clock = clock.Add(time.Second)
		// An entry stored before the caller's task read is not fresh.
		if read(task, clock) || *calls != 1 {
			t.Fatalf("warm read calls=%d", *calls)
		}
		// An entry stored at or after the caller's task read is fresh.
		if !read(task, clock.Add(-2*time.Second)) || *calls != 1 {
			t.Fatalf("entry newer than the read: calls=%d", *calls)
		}
		for _, changed := range []core.Task{
			{ID: "task-a", State: core.TaskApproved, ApprovedHeadSHA: "head-2"},
			{ID: "task-a", State: core.TaskApproved, ApprovedHeadSHA: "head-2", MergeApproval: true},
			{ID: "task-a", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, ReviewedHeadSHA: "head-2", MergeApproval: true},
			{ID: "task-b", State: core.TaskApproved, ApprovedHeadSHA: "head-2"},
		} {
			before := *calls
			read(changed, clock)
			if *calls != before+1 {
				t.Fatalf("key change %+v reused another key's entry", changed)
			}
		}
		before := *calls
		read(task, clock)
		otherWorkspace := store.WithWorkspace(t.Context(), "other")
		if _, _, err := cache.readObserved(otherWorkspace, task, hook, clock); err != nil || *calls != before+2 {
			t.Fatalf("workspace collision: calls=%d err=%v", *calls, err)
		}
		before = *calls
		clock = clock.Add(taskRunMergeReadinessReuse)
		read(task, clock)
		if *calls != before+1 {
			t.Fatal("an expired entry was reused")
		}
		failures, failing := immediate(func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
			return dispatch.MergeReadiness{}, errors.New("forge unavailable")
		})
		fail := core.Task{ID: "task-fail", State: core.TaskApproved, ApprovedHeadSHA: "head-1"}
		for i := 0; i < 2; i++ {
			if _, _, err := cache.readObserved(ctx, fail, failing, clock); err == nil {
				t.Fatal("failure answered success")
			}
		}
		if *failures != 2 || cache.lastKnown(ctx, fail) != nil {
			t.Fatalf("failure was cached: calls=%d", *failures)
		}
	})

	t.Run("CancelledWaiterReturnsAlone", func(t *testing.T) {
		cache := &taskRunReadinessCache{clock: func() time.Time { return now }}
		hook := newGatedHook(mergeable)
		leader := make(chan error, 1)
		go func() {
			_, _, err := cache.readObserved(ctx, task, hook.call, now)
			leader <- err
		}()
		<-hook.entered
		waiterCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := cache.readObserved(waiterCtx, task, hook.call, now); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter err = %v", err)
		}
		close(hook.release)
		if err := <-leader; err != nil {
			t.Fatal(err)
		}
		if cache.lastKnown(ctx, task) == nil || hook.count() != 1 {
			t.Fatalf("leader result not stored or extra calls: %d", hook.count())
		}
	})

	t.Run("LeaderCancellationLetsLiveWaiterCall", func(t *testing.T) {
		cache := &taskRunReadinessCache{clock: func() time.Time { return now }}
		hook := newGatedHook(mergeable)
		leaderCtx, cancelLeader := context.WithCancel(ctx)
		leader := make(chan error, 1)
		go func() {
			_, _, err := cache.readObserved(leaderCtx, task, hook.call, now)
			leader <- err
		}()
		<-hook.entered
		waiter := make(chan error, 1)
		go func() {
			_, fresh, err := cache.readObserved(ctx, task, hook.call, now)
			if err == nil && !fresh {
				err = errors.New("waiter result not fresh")
			}
			waiter <- err
		}()
		cancelLeader()
		if err := <-leader; !errors.Is(err, context.Canceled) {
			t.Fatalf("leader err = %v", err)
		}
		<-hook.entered
		close(hook.release)
		if err := <-waiter; err != nil {
			t.Fatal(err)
		}
		if hook.count() != 2 {
			t.Fatalf("hook calls = %d, want the waiter's own call", hook.count())
		}
	})

	t.Run("ForgetKeepsOldFlightOut", func(t *testing.T) {
		cache := &taskRunReadinessCache{clock: func() time.Time { return now }}
		hook := newGatedHook(mergeable)
		leader := make(chan error, 1)
		go func() {
			_, _, err := cache.readObserved(ctx, task, hook.call, now)
			leader <- err
		}()
		<-hook.entered
		cache.forget(ctx, task.ID)
		close(hook.release)
		if err := <-leader; err != nil {
			t.Fatal(err)
		}
		if cache.lastKnown(ctx, task) != nil {
			t.Fatal("a flight from before forget stored its result")
		}
	})
}

// mutatingHookStore reports the post-hook task state once the hook has run,
// as a readiness read that begins a refresh review would.
type mutatingHookStore struct {
	store.Store
	mu     sync.Mutex
	mutate bool
}

func (m *mutatingHookStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	task, err := m.Store.GetTask(ctx, id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil && m.mutate {
		task.State = core.TaskRunning
	}
	return task, err
}

func TestTaskDetailReadinessThroughRoute(t *testing.T) {
	f := newDetailFixture(t, 1)
	d := newDetailServer(f)
	failures := 0
	d.server.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		failures++
		return dispatch.MergeReadiness{}, errors.New("forge unavailable")
	}
	// An approved task without readiness answers 503.
	if response := d.get(t, detailPath(f.branches["approved"], false)); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("approved failure: %d %s", response.Code, response.Body)
	}
	// A failed gate peek renders without merge_readiness or a merge choice.
	response := d.get(t, detailPath(f.branches["gate"], true))
	if response.Code != http.StatusOK {
		t.Fatalf("gate failure: %d %s", response.Code, response.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["merge_readiness"]; present {
		t.Fatalf("gate failure carried readiness: %s", body["merge_readiness"])
	}
	if failures != 2 {
		t.Fatalf("hook calls = %d; failures must not be cached", failures)
	}
	// The projection then offers no merge choice; a successful peek does.
	nextAction := func() string {
		t.Helper()
		response := d.get(t, "/v1/tasks/"+f.branches["gate"]+"/next-action?workspace_id=demo")
		if response.Code != http.StatusOK {
			t.Fatalf("next action: %d %s", response.Code, response.Body)
		}
		return response.Body.String()
	}
	if action := nextAction(); strings.Contains(action, "/merge\"") {
		t.Fatalf("failed peek offered the merge choice: %s", action)
	}
	d.server.OnMergeReadiness = func(_ context.Context, task core.Task) (dispatch.MergeReadiness, error) {
		return dispatch.MergeReadiness{State: "MERGEABLE", HeadSHA: task.ReviewedHeadSHA}, nil
	}
	if response := d.get(t, detailPath(f.branches["gate"], true)); response.Code != http.StatusOK {
		t.Fatalf("gate success: %d", response.Code)
	}
	if action := nextAction(); !strings.Contains(action, "/merge\"") {
		t.Fatalf("successful peek offered no merge choice: %s", action)
	}
	// A hook that writes task state yields the post-hook task in the body.
	mutating := &mutatingHookStore{Store: f.st}
	server := NewServer(mutating)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	server.OnMergeReadiness = func(_ context.Context, task core.Task) (dispatch.MergeReadiness, error) {
		mutating.mu.Lock()
		mutating.mutate = true
		mutating.mu.Unlock()
		return dispatch.MergeReadiness{State: "STALE", HeadSHA: "moved"}, nil
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, detailPath(f.branches["approved"], false), nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	server.Handler().ServeHTTP(recorder, request)
	var item struct {
		Task           core.Task                `json:"task"`
		MergeReadiness *dispatch.MergeReadiness `json:"merge_readiness"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &item) != nil || item.Task.State != core.TaskRunning || item.MergeReadiness == nil || item.MergeReadiness.State != "STALE" {
		t.Fatalf("post-hook state: %d %s", recorder.Code, recorder.Body)
	}
}
