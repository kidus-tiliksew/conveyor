// Package httpapi is the Phase 2 control-plane REST + SSE surface and embedded
// activity/review SPA (component-http-api). Mutations are authenticated and
// recorded with actor identity in the append-only event stream.
package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type Server struct {
	mcpReads mcpReadCache
	// documentVersions holds parsed immutable document versions for section
	// reads and search (mcp_document_cache.go).
	documentVersions mcpDocumentCache
	// mergeReadiness bounds merge-readiness reads of task detail and next
	// actions (component-http-api, Request-scoped detail reads).
	mergeReadiness mergeReadinessCache
	Store          store.Store
	Credentials    CredentialVerifier
	// Release is the build-injected binary identity reported by /v1/version.
	Release                  string
	planningBundleMu         sync.Mutex
	planningBundleDispatched map[string]struct{}
	passwordLimiterOnce      sync.Once
	passwordLimiter          *passwordAttemptLimiter
	// historyCursors signs task history cursors under a key drawn once per
	// process; tests may install a codec before first use (task 261015-3721c6).
	historyCursorsOnce sync.Once
	historyCursors     *taskHistoryCursorCodec
	historyCursorsErr  error
	// taskEventStreamTicks is an extra wake source for the notify stream in
	// tests: each tick makes it read its fingerprint as a notice would.
	taskEventStreamTicks func() (<-chan time.Time, func())
	// Changes is the backend's change-notice hub. Task event streams and the
	// workspace change stream subscribe to it and read only on a notice
	// (component-http-api, "When a task stream reads"; DEC-99(1)). NewServer
	// sets it from a store that exposes Changes().
	Changes *changes.Hub
	// StreamKeepalive replaces the 25-second keep-alive ticker of every
	// stream in tests; nil selects time.NewTicker.
	StreamKeepalive func() (<-chan time.Time, func())
	// Repos is the set of valid repo names; nil skips validation.
	Repos []string
	// multipartTaskIntakeLimit overrides maxMultipartTaskIntakeBytes in tests;
	// zero selects the production bound.
	multipartTaskIntakeLimit int64
	// OnCreate is invoked with each created task's ID (the dispatcher's
	// Enqueue). Nil means tasks queue without dispatch (tests).
	OnCreate func(context.Context, string)
	// GenerateTaskTitle uses the trusted control-plane AI integration for every
	// task intake. Nil fails closed instead of persisting an untitled task.
	GenerateTaskTitle func(context.Context, core.Task) (string, error)
	// OnIntervention validates fresh authoritative state and advances the gate.
	// Plan-revision decisions are committed first because their dispatched
	// work-order context is derived from the durable intervention record.
	OnIntervention func(context.Context, core.Task, core.Job, core.Intervention) error
	// OnMerge performs and authoritatively confirms the final forge merge.
	OnMerge func(context.Context, core.Task, core.Actor) error
	// OnGateMerge is the one-act operator merge at the merge_approval gate,
	// run as the acting operator (DEC-64(1); component-submission-merge).
	OnGateMerge      func(context.Context, core.Task, core.Actor) (core.Task, error)
	OnMergeReadiness func(context.Context, core.Task) (dispatch.MergeReadiness, error)
	OnConflictFix    func(context.Context, core.Task) (core.WorkOrder, error)
	// Workspace stamps created tasks.
	Workspace string
	// BearerToken authenticates mutating requests (component-http-api). An empty
	// token denies all mutations rather than silently disabling auth.
	BearerToken string
	// WorkspaceInfo is the static fallback for the display snapshot; production
	// resolves it dynamically through ConfigProvider.
	WorkspaceInfo *WorkspaceInfo
	// ConfigProvider resolves current database-backed workspace scope while
	// preserving file-backed deployment fields.
	ConfigProvider func(context.Context) (*config.Config, error)
	ConfigStore    WorkspaceConfigStore
	Workspaces     store.WorkspaceControlStore
	Memberships    store.MembershipStore
	// ActorOwners resolves an agent:<id> actor to its owner on a read
	// (req-security-boundaries AC-1.3).
	ActorOwners         store.AgentActorOwnerStore
	IdentityProvisioner store.IdentityProvisioner
	CallerIdentities    store.CallerIdentityStore
	OwnProfiles         store.OwnProfileStore
	PersonalTokens      store.PersonalAccessTokenStore
	WorkspaceGitHubApps store.WorkspaceGitHubAppStore
	GitHubApps          *github.AppClient
	// GitHubWebhooks stores the webhook secret and accepted deliveries, and
	// GitHubWebhookApplier applies each accepted delivery
	// (component-http-api, GitHub webhook receiver).
	GitHubWebhooks       store.GitHubWebhookStore
	GitHubWebhookApplier GitHubWebhookApplier
	// GitHubSilenceFallback reports whether this daemon's timed GitHub
	// silence fallback runs for a workspace (component-identity-membership;
	// req-quiet-idle-runtime AC-3.8); nil reports false.
	GitHubSilenceFallback func(workspace string) bool
	// verificationKitMemo caches exact-commit kit discovery for the workspace
	// kit registry (component-verification-kit-contract).
	verificationKitMemo   kitRegistryMemo
	appStates             manifestStates
	appSetupStates        manifestStates
	InvitationSessions    store.InvitationSessionStore
	InvitationDelivery    config.InvitationDelivery
	EnsureWorkspaceQueues func(string, *config.Config) error
	Deployment            *config.Config
	WorkOrders            *workorder.Service
	Monitor               *monitor.Service
	Planning              *planning.Service

	// requirementInsightLocks serializes headline generation per workspace
	// and requirement in this process (feature-requirement-insights RI-5).
	requirementInsightLocks requirementInsightLocks
}

type CredentialVerifier interface {
	VerifyCredential(context.Context, string) (core.AuthenticatedCredential, error)
}

func NewServer(s store.Store) *Server {
	server := &Server{Store: s, Release: releaseinfo.Version}
	if credentials, ok := s.(CredentialVerifier); ok {
		server.Credentials = credentials
	}
	if memberships, ok := s.(store.MembershipStore); ok {
		server.Memberships = memberships
	}
	if owners, ok := s.(store.AgentActorOwnerStore); ok {
		server.ActorOwners = owners
	}
	if identities, ok := s.(store.IdentityProvisioner); ok {
		server.IdentityProvisioner = identities
	}
	if identities, ok := s.(store.CallerIdentityStore); ok {
		server.CallerIdentities = identities
	}
	if profiles, ok := s.(store.OwnProfileStore); ok {
		server.OwnProfiles = profiles
	}
	if tokens, ok := s.(store.PersonalAccessTokenStore); ok {
		server.PersonalTokens = tokens
	}
	if apps, ok := s.(store.WorkspaceGitHubAppStore); ok {
		server.WorkspaceGitHubApps = apps
	}
	if hooks, ok := s.(store.GitHubWebhookStore); ok {
		server.GitHubWebhooks = hooks
	}
	if sessions, ok := s.(store.InvitationSessionStore); ok {
		server.InvitationSessions = sessions
	}
	if source, ok := s.(interface{ Changes() *changes.Hub }); ok {
		server.Changes = source.Changes()
	}
	return server
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, githubAppRequestLogger, s.serverTiming, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"version": s.Release})
		})
		// GitHub authenticates a delivery with its signature; the route
		// reads no credential (component-http-api, GitHub webhook receiver).
		r.Post("/github/webhook/{workspace}", s.receiveGitHubWebhook)
		r.Post("/sign-in/redeem", s.redeemSignInLink)
		r.Post("/sign-in/password", s.signInWithPassword)
		r.With(s.requireSelfServiceCredential).Post("/sign-out", s.signOutDashboardSession)
		r.With(s.requireSelfServiceCredential).Post("/password", s.setOwnPassword)
		r.With(s.requireWorkspaceAuth).Get("/workspaces", s.listWorkspaces)
		r.With(s.requireSelfServiceCredential, s.resolveOptionalWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/me", s.getCallerIdentity)
		r.With(s.requireSelfServiceCredential).Put("/me", s.putOwnDisplayName)
		// Instance administration (DEC-75(2), DEC-75(5)): only the deployment
		// marker owner's own user credential reaches account provisioning;
		// every other authenticated human, a workspace operator included, gets
		// the uniform 404. Workspace creation is mounted below with
		// requireWorkspaceCreation, which also admits an active operator of any
		// workspace and whose single policy function also serves /v1/me
		// can_create_workspaces (req-accounts-and-membership AC-1.4).
		r.With(s.requireInstanceAdministration).Post("/users", s.provisionIdentityUser)
		// Self-service credential routes carry no subject in the path: the owner
		// is the presented credential, so no capability applies and no request
		// can name another user's tokens (REQ-2/AC-2.1).
		r.With(s.requireSelfServiceCredential).Get("/tokens", s.listOwnPersonalAccessTokens)
		r.With(s.requireSelfServiceCredential).Post("/tokens", s.issueOwnPersonalAccessToken)
		r.With(s.requireSelfServiceCredential).Delete("/tokens/{token_id}", s.revokeOwnPersonalAccessToken)
		r.With(s.requireWorkspaceCreation).Post("/workspaces", s.createWorkspace)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}", s.getWorkspaceRecord)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/config", s.getWorkspaceConfig)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/changes", s.streamWorkspaceChanges)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Put("/workspaces/{workspace_id}/config", s.putWorkspaceConfig)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Post("/workspaces/{workspace_id}/github-app/manifest", s.createWorkspaceGitHubAppManifest)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Get("/workspaces/{workspace_id}/github-app/callback", s.workspaceGitHubAppCallback)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Get("/workspaces/{workspace_id}/github-app/setup", s.workspaceGitHubAppSetup)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/github-app", s.getWorkspaceGitHubApp)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/verification-kits", s.getWorkspaceVerificationKits)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Delete("/workspaces/{workspace_id}/github-app", s.deleteWorkspaceGitHubApp)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Post("/workspaces/{workspace_id}/github-app/webhook-secret", s.createWorkspaceGitHubWebhookSecret)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/members", s.listWorkspaceMembers)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Post("/workspaces/{workspace_id}/members", s.grantWorkspaceMembership)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Get("/workspaces/{workspace_id}/invitations", s.listWorkspaceInvitations)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Delete("/workspaces/{workspace_id}/invitations/{email}", s.revokeWorkspaceInvitation)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Post("/workspaces/{workspace_id}/invitations/{email}/resend", s.resendWorkspaceInvitation)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Delete("/workspaces/{workspace_id}/members/{user_id}", s.revokeWorkspaceMembership)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace))
			r.Get("/activity", s.listActivity)
			r.Get("/attention/tasks", s.listCallerAttentionTasks)
			r.Get("/pending-proposals", s.listPendingProposals)
			r.Get("/tasks", s.listTasks)
			r.Get("/task-operations", s.listTaskOperations)
			r.Get("/task-map", s.getTaskMap)
			r.Get("/tasks/{id}", s.getTask)
			r.Get("/tasks/{id}/next-action", s.getNextAction)
			r.Get("/tasks/{id}/flow", s.getTaskFlow)
			r.Get("/inbox", s.listInbox)
			r.Get("/next-actions", s.listNextActions)
			r.Get("/inbox/acts", s.listFactoryActs)
			r.Get("/tasks/{id}/jobs", s.listJobs)
			r.Get("/tasks/{id}/events", s.listEvents)
			r.Get("/tasks/{id}/events/stream", s.streamEvents)
			r.Get("/tasks/{id}/activity", s.getTaskActivity)
			r.Get("/tasks/{id}/history", s.getTaskHistory)
			r.Get("/tasks/{id}/verification", s.getTaskVerification)
			r.Get("/tasks/{id}/verification/contexts/{context_id}/{collection}", s.getTaskVerificationPage)
			r.Get("/tasks/{id}/verification/contexts/{context_id}/evidence/{evidence_id}", s.getTaskVerificationEvidence)
			r.Get("/tasks/{id}/verification/contexts/{context_id}/evidence/{evidence_id}/artifacts/{artifact_id}", s.getTaskVerificationEvidence)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/verification/observations", s.recordTaskVerificationObservation)
			r.Get("/tasks/{id}/audit/{kind}/{record_id}", s.getTaskAudit)
			r.Get("/lineage/{type}/{id}", s.getLineage)
			r.Get("/lineage/{type}/{id}/impact", s.getLineageImpact)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/lineage/rebuild", s.rebuildLineage)
			r.Get("/tasks/{id}/interventions", s.listInterventions)
			r.Get("/tasks/{id}/spec", s.getLatestSpec)
			r.With(s.requireUserBearerAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Get("/tasks/{id}/worktree-cleanup", s.getWorktreeCleanupStatus)
			r.With(s.requireUserBearerAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/worktree-cleanup", s.recordWorktreeCleanup)
			// Request-changes is the human merge-gate action. Dashboard sessions
			// retain this route through the outer CSRF proof and capability
			// boundary.
			r.With(s.requireTaskRequestChangesAuth, s.requireWorkspaceCapability(core.CapabilityRequestChanges)).Post("/tasks/{id}/request-changes", s.requestTaskChanges)
			r.Get("/reviews", s.listReviews)
			r.Get("/workspace", s.getWorkspace)
			r.Get("/work-orders", s.listWorkOrders)
			r.Get("/work-orders/{id}", s.getWorkOrder)
			r.Get("/monitor", s.getMonitorStatus)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/work-orders/{id}/recover", s.recoverWorkOrder)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/work-orders/{id}/preempt", s.preemptWorkOrder)
			r.Get("/blueprints", s.listBlueprints)
			r.Get("/requirements", s.listRequirements)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/requirements", s.createRequirement)
			r.Get("/requirements/{id}", s.getRequirement)
			r.Get("/requirements/{id}/events", s.listRequirementEventPage)
			r.Get("/requirements/{id}/versions", s.listRequirementVersions)
			r.Get("/requirements/{id}/checkpoint-context-candidates", s.listCheckpointContextCandidates)
			// Requirement insights (feature-requirement-insights RI-6; DEC-87
			// (6)): reads need view_workspace, generation needs create_tasks.
			r.Get("/requirements/{id}/insights/summary", s.getRequirementInsightSummary)
			r.With(s.requireMutationCapability(core.CapabilityCreateTasks)).Post("/requirements/{id}/insights/summary", s.generateRequirementInsightSummary)
			r.Get("/requirements/{id}/insights/progress", s.getRequirementInsightProgress)
			r.Get("/requirements/{id}/insights/documents", s.getRequirementInsightDocuments)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/requirements/{id}/versions", s.proposeRequirementVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/requirements/{id}/versions/{version}/confirm", s.confirmRequirementVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/requirements/{id}/versions/{version}/dismiss", s.dismissRequirementVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/requirements/{id}/archive", s.archiveRequirement)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/requirements/{id}/restore", s.restoreRequirement)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/requirements/{id}/staleness/{signal}/acknowledge", s.acknowledgeRequirementStaleness)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/requirements/{id}/staleness/{signal}/follow-up", s.createRequirementStalenessFollowUp)
			r.Get("/reference-documents", s.listReferenceDocuments)
			r.With(s.requireMutationCapability(core.CapabilityManageReferenceDocuments)).Post("/reference-documents", s.createReferenceDocument)
			r.Get("/reference-documents/{id}/versions", s.listReferenceDocumentVersions)
			r.With(s.requireMutationCapability(core.CapabilityManageReferenceDocuments)).Post("/reference-documents/{id}/versions", s.supersedeReferenceDocument)
			r.With(s.requireMutationCapability(core.CapabilityManageReferenceDocuments)).Delete("/reference-documents/{id}", s.deleteReferenceDocument)
			r.Get("/system-designs", s.listSystemDesigns)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/system-designs", s.createSystemDesign)
			r.Get("/system-designs/{id}", s.getSystemDesign)
			r.Get("/system-designs/{id}/events", s.listSystemDesignEventPage)
			r.Get("/system-designs/{id}/versions", s.listSystemDesignVersions)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/system-designs/{id}/versions", s.proposeSystemDesignVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/system-designs/{id}/versions/{version}/confirm", s.confirmSystemDesignVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/system-designs/{id}/versions/{version}/dismiss", s.dismissSystemDesignVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/system-designs/{id}/archive", s.archiveSystemDesign)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/system-designs/{id}/restore", s.restoreSystemDesign)
			r.Get("/decisions", s.listDecisions)
			r.Get("/decision-citations", s.listDecisionCitations)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/decisions", s.proposeDecision)
			r.Get("/decisions/{id}", s.getDecision)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/decisions/{id}/confirm", s.confirmDecision)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/decisions/{id}/dismiss", s.dismissDecision)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/decisions/{id}/sweep/{tier}/{document_id}/dismiss", s.dismissDecisionSupersessionSweep)
			r.Get("/planning-sessions", s.listPlanningSessions)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/planning-sessions", s.createPlanningSession)
			r.Get("/planning-sessions/{id}", s.getPlanningSession)
			r.Get("/planning-sessions/{id}/messages", s.listPlanningMessages)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/planning-sessions/{id}/messages", s.streamPlanningMessage)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/planning-sessions/{id}/chat", s.streamPlanningMessage)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/planning-sessions/{id}/abandon", s.abandonPlanningSession)
			r.Get("/planning-bundles", s.listPlanningBundles)
			r.Get("/planning-bundles/{id}", s.getPlanningBundle)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/planning-bundles/{id}/approve", s.approvePlanningBundle)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/planning-bundles/{id}/reject", s.rejectPlanningBundle)
			r.Get("/lifecycle-diagram", s.getLifecycleDiagram)
			r.Get("/workspace/config", s.getWorkspaceConfig)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Put("/workspace/config", s.putWorkspaceConfig)
			r.With(s.requireMutationCapability(core.CapabilityCreateTasks)).Post("/tasks", s.createTask)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/monitor/observations", s.observeMonitorSignal)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/monitor/drift/{id}/resolve", s.resolveMonitorDrift)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/redispatch", s.redispatchTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Put("/tasks/{id}/hold", s.retiredTaskHold)
			r.With(s.requireMutationCapability(core.CapabilitySetAssignee)).Put("/tasks/{id}/assignee", s.setTaskAssignee)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/context", s.updateTaskContext)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/context/proposals/{kind}/{target_id}/confirm", s.confirmTaskContextProposal)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/context/proposals/{kind}/{target_id}/dismiss", s.dismissTaskContextProposal)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/setup", s.changeTaskSetup)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/policy", s.changeTaskPolicy)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/dependencies", s.addTaskDependency)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Delete("/tasks/{id}/dependencies/{dependency_id}", s.removeTaskDependency)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/review-round/retry", s.retryReviewRound)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/review-round/seats", s.addReviewSeats)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/review-round/recover", s.recoverInterruptedReviewRound)
			r.With(s.requireMutationCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/review-rounds/{round}/summaries", s.recordReviewSummary)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/review", s.reviewTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/close", s.closeTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/restart", s.restartTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/branch", s.attachTaskBranch)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/merge", s.mergeTask)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/merge-conflict-fix", s.fixMergeConflict)
			r.Get("/artifacts", s.listArtifacts)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/artifacts", s.uploadArtifact)
			r.With(requireExplicitArtifactWorkspace, s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/artifacts/{id}/metadata-repair", s.repairArtifactMetadata)
			r.Get("/artifacts/{id}", s.downloadArtifact)
		})
	})
	// All methods route to the handler so non-POST gets a spec-correct 405
	// (streamable-HTTP clients probe GET for an SSE stream); registering
	// only Post would let GET fall through to the SPA catch-all as 200 HTML.
	r.With(s.requireMCPAuth).HandleFunc("/mcp", s.handleMCP)
	r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace), s.requireMutationCapability(core.CapabilityOperateGates)).Post("/v1/work-orders/{id}/verification/permissions", s.grantVerificationPermissions)
	// The operator grant projection is user-only and registered before the
	// generic MCP-authenticated operation routes (component-verification-runner).
	r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace), s.requireWorkspaceCapability(core.CapabilityOperateGates)).Get("/v1/work-orders/{id}/verification/permissions", s.getVerificationPermissions)
	r.With(s.requireMCPAuth).Post("/v1/work-orders/{id}/verification/{operation}", s.verificationOrder)
	r.With(s.requireMCPAuth).Get("/v1/work-orders/{id}/verification/{operation}", s.verificationOrder)
	r.With(s.requireMCPAuth).Get("/v1/work-orders/{id}/pull-request-template", s.getSubmissionTemplate)
	r.With(s.requireMCPAuth).Post("/v1/work-orders/{id}/submit-for-review", s.submitWorkOrderReview)
	r.With(s.requireMCPAuth).Post("/v1/work-orders/{id}/context-refresh", s.refreshWorkOrderContext)
	r.Get("/", serveDashboard)
	// The SPA router owns all non-API paths; adding a client route no longer
	// requires duplicating it in the Go server.
	r.Get("/*", func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v1/") {
			http.NotFound(w, request)
			return
		}
		serveDashboard(w, request)
	})
	return r
}

func (s *Server) redispatchTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	// core.NextActionRedispatchRefusal states these same refusals to the next
	// action, so a choice is offered only when this route would accept it
	// (req-operator-next-action AC-1.4; component-next-action, Invariants 7).
	if t.State == core.TaskRunning {
		http.Error(w, core.TaskRedispatchActiveRefusal, http.StatusConflict)
		return
	}
	if t.State == core.TaskQueued {
		if err := s.Store.EnsureTaskEnqueued(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if errors.Is(err, store.ErrTaskHeldByWorkOrder) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeStoreFailure(w, r, err)
			return
		}
	} else {
		nextStage := core.TaskRedispatchStage(t)
		if nextStage == "" {
			http.Error(w, core.TaskRedispatchNoStageRefusal, http.StatusConflict)
			return
		}
		command := core.TaskRedispatchCommand(t.State)
		var transitionErr error
		if t.State == core.TaskParked {
			// A triage park records the stage in RecoveryStage and clears
			// NextStage. Recovery restores that stage as the next dispatch
			// target (component-http-api).
			_, transitionErr = taskops.New(s.Store).Perform(r.Context(), id, taskops.Command{Kind: command, NextStage: nextStage, ProjectStages: true})
		} else {
			_, transitionErr = taskops.New(s.Store).Perform(r.Context(), id, taskops.Command{Kind: command})
		}
		if transitionErr != nil {
			var invalidTransition *core.ErrInvalidTransition
			if errors.As(transitionErr, &invalidTransition) {
				http.Error(w, transitionErr.Error(), http.StatusConflict)
				return
			}
			writeStoreFailure(w, r, transitionErr)
			return
		}
		t.State = core.TaskQueued
		t.NextStage = nextStage
		if command == core.TaskRecover {
			t.RecoveryStage = ""
		}
	}
	if s.OnCreate != nil {
		s.OnCreate(r.Context(), id)
	}
	writeJSON(w, http.StatusAccepted, t)
}

// retiredTaskHold answers the retired per-task hold route. A task carries no
// hold or other worker reservation (DEC-74(3)); the route keeps its
// authentication and capability middleware, reads no body, looks up no task,
// writes nothing, and appends no event (component-http-api).
func (s *Server) retiredTaskHold(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": taskHoldRetired, "message": taskHoldRetiredRouteMessage})
}

func (s *Server) setTaskAssignee(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AssigneeUserID string `json:"assignee_user_id"`
	}
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	task, err := taskops.New(s.Store).SetAssignee(r.Context(), chi.URLParam(r, "id"), request.AssigneeUserID)
	if err != nil {
		writeClassifiedStoreError(w, r, err,
			refuseSentinel(store.ErrNotFound, http.StatusBadRequest, "task not found"),
			refuseSentinel(store.ErrAssigneeNotMember, http.StatusBadRequest, "assignee is not an active member of the workspace"),
			refuseSentinel(store.ErrAssigneeLacksCapability, http.StatusBadRequest, "assignee role lacks the claim_work capability"))
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) requireMutationCapability(capability core.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential, ok := store.CredentialFromContext(r.Context())
			if !ok {
				var err error
				credential, err = s.authenticateHumanCredential(w, r)
				if err != nil {
					writeCredentialVerificationError(w, err)
					return
				}
				ok = true
			}
			if !ok || credential.Kind != core.CredentialUser {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !s.requireSessionMutationProof(w, r, credential) {
				return
			}
			ctx := store.WithCredential(r.Context(), credential)
			ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
			if workspaceID, scoped := store.WorkspaceFromContext(ctx); scoped && s.Workspaces != nil {
				if s.Memberships == nil {
					writeWorkspaceNotFound(w)
					return
				}
				allowed, err := s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspaceID, capability)
				if err != nil {
					log.Printf("authorize workspace mutation: %v", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				if !allowed {
					writeWorkspaceNotFound(w)
					return
				}
			} else {
				if credential.Scope != core.CredentialScopeOperator {
					w.Header().Set("WWW-Authenticate", "Bearer")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				// Memory-only tests and explicit non-durable deployments retain
				// the configured-token fallback. Durable stores always expose the
				// live membership authority through NewServer.
				if s.Credentials != nil && s.Memberships != nil {
					allowed, err := s.Memberships.AuthorizeDeployment(ctx, credential.OwnerUserID, capability)
					if err != nil {
						log.Printf("authorize deployment mutation: %v", err)
						http.Error(w, "internal server error", http.StatusInternalServerError)
						return
					}
					if !allowed {
						w.Header().Set("WWW-Authenticate", "Bearer")
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (s *Server) requireWorkspaceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated, ok := s.withHumanCredential(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, authenticated)
	})
}

// withHumanCredential keeps the workspace-read protocol boundary authenticated
// even when an explicit memory deployment has no workspace registry
// (req-security-boundaries REQ-1; component-http-api).
func (s *Server) withHumanCredential(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok {
		var err error
		credential, err = s.authenticateHumanCredential(w, r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return r, false
		}
	}
	if credential.Kind != core.CredentialUser {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return r, false
	}
	if !s.requireSessionMutationProof(w, r, credential) {
		return r, false
	}
	ctx := store.WithCredential(r.Context(), credential)
	ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
	return r.WithContext(ctx), true
}

// requireSelfServiceCredential authenticates the human credential that owns a
// self-service request. It never resolves a workspace and never consults a
// capability bundle: the caller's authority over the resource is the fact that
// the resource is theirs.
func (s *Server) requireSelfServiceCredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, err := s.authenticateHumanCredential(w, r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return
		}
		if credential.Kind != core.CredentialUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.requireSessionMutationProof(w, r, credential) {
			return
		}
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireWorkspaceCapability(capability core.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.Workspaces == nil {
				authenticated, ok := s.withHumanCredential(w, r)
				if !ok {
					return
				}
				next.ServeHTTP(w, authenticated)
				return
			}
			if s.Memberships == nil {
				writeWorkspaceNotFound(w)
				return
			}
			credential, ok := store.CredentialFromContext(r.Context())
			workspaceID, scoped := store.WorkspaceFromContext(r.Context())
			if !ok || !scoped {
				writeWorkspaceNotFound(w)
				return
			}
			allowed, err := s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, workspaceID, capability)
			if err != nil {
				log.Printf("authorize workspace capability: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !allowed {
				writeWorkspaceNotFound(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeWorkspaceNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found", "message": "workspace not found"})
}

func (s *Server) requireMCPAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		credential, verifyErr := s.verifyCredential(r.Context(), provided)
		// Every credential is a person's, so anything else is refused like an
		// unknown one and never stamped as a user's act (component-http-api
		// invariant 4; DEC-116(1)).
		if verifyErr == nil && credential.Kind != core.CredentialUser {
			verifyErr = core.ErrInvalidCredential
		}
		if verifyErr == nil {
			// A session acts as its person, so its acts are that person's
			// (req-accounts-and-membership AC-3.2; DEC-116(2)).
			actor := store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser}
			ctx := store.WithCredential(r.Context(), credential)
			ctx = store.WithActor(ctx, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if !errors.Is(verifyErr, core.ErrInvalidCredential) {
			writeCredentialVerificationError(w, verifyErr)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (s *Server) authenticateUserCredential(r *http.Request) (core.AuthenticatedCredential, error) {
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return core.AuthenticatedCredential{}, core.ErrInvalidCredential
	}
	return s.verifyCredential(r.Context(), provided)
}

func (s *Server) authenticateHumanCredential(w http.ResponseWriter, r *http.Request) (core.AuthenticatedCredential, error) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		credential, err := s.authenticateUserCredential(r)
		if err == nil && credential.Method == "" {
			credential.Method = core.CredentialMethodBearer
		}
		return credential, err
	}
	if s.InvitationSessions == nil {
		return core.AuthenticatedCredential{}, core.ErrInvalidCredential
	}
	cookie, err := r.Cookie(dashboardSessionCookie)
	if err != nil || cookie.Value == "" {
		return core.AuthenticatedCredential{}, core.ErrInvalidCredential
	}
	credential, err := s.InvitationSessions.VerifyDashboardSession(r.Context(), cookie.Value)
	// SessionExpiresAt is nonzero only when this verification's refresh won,
	// so the cookie is re-set exactly when the stored expiry moved
	// (component-identity-membership).
	if err == nil && !credential.SessionExpiresAt.IsZero() {
		setDashboardSessionCookie(w, r, s.InvitationDelivery.PublicURL, cookie.Value, credential.SessionExpiresAt)
	}
	return credential, err
}

func (s *Server) requireSessionMutationProof(w http.ResponseWriter, r *http.Request, credential core.AuthenticatedCredential) bool {
	if credential.Method != core.CredentialMethodSession || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if r.Header.Get("X-Conveyor-CSRF") != "1" {
		http.Error(w, "CSRF proof required", http.StatusForbidden)
		return false
	}
	return s.requireRequestOrigin(w, r)
}

func (s *Server) requireRequestOrigin(w http.ResponseWriter, r *http.Request) bool {
	expectedOrigin := s.InvitationDelivery.PublicURL
	if expectedOrigin == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		expectedOrigin = scheme + "://" + r.Host
	}
	if origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/"); origin == "" || origin != expectedOrigin {
		http.Error(w, "invalid request origin", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) verifyCredential(ctx context.Context, provided string) (core.AuthenticatedCredential, error) {
	if s.Credentials != nil {
		return s.Credentials.VerifyCredential(ctx, provided)
	}
	// Memory-only tests and explicit non-durable deployments have no identity
	// registry. Their configured shared token models the seeded legacy PAT.
	if s.BearerToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(s.BearerToken)) == 1 {
		return core.AuthenticatedCredential{ID: "legacy", OwnerUserID: "local-operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}, nil
	}
	return core.AuthenticatedCredential{}, core.ErrInvalidCredential
}

func writeCredentialVerificationError(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrInvalidCredential) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "credential verification failed", http.StatusInternalServerError)
}

type reviewRequest struct {
	Action     core.InterventionAction `json:"action"`
	ReasonCode string                  `json:"reason_code"`
	Comment    string                  `json:"comment"`
}

type closeTaskRequest struct {
	Reason     string `json:"reason"`
	ReasonCode string `json:"reason_code"`
	Comment    string `json:"comment"`
}

type removeTaskDependencyRequest struct {
	Reason    string `json:"reason"`
	RequestID string `json:"request_id"`
}

type addTaskDependencyRequest struct {
	DependsOnTaskID string `json:"depends_on_task_id"`
	Reason          string `json:"reason"`
	RequestID       string `json:"request_id"`
}

func (s *Server) addTaskDependency(w http.ResponseWriter, r *http.Request) {
	var request addTaskDependencyRequest
	if err := decodeStrictBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	result, err := s.Store.AddTaskDependency(r.Context(), store.DependencyAdditionRequest{
		TaskID: chi.URLParam(r, "id"), DependsOnTaskID: request.DependsOnTaskID,
		Reason: request.Reason, RequestID: request.RequestID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, store.ErrTaskTerminal), errors.Is(err, store.ErrTaskDependencyCycle), errors.Is(err, store.ErrTaskDependencyConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrTaskDependencyInvalid):
			http.Error(w, store.ErrTaskDependencyInvalid.Error(), http.StatusBadRequest)
		default:
			writeStoreFailure(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) removeTaskDependency(w http.ResponseWriter, r *http.Request) {
	var request removeTaskDependencyRequest
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	result, err := s.Store.RemoveTaskDependency(r.Context(), store.DependencyRemovalRequest{
		TaskID: chi.URLParam(r, "id"), DependsOnTaskID: chi.URLParam(r, "dependency_id"),
		Reason: request.Reason, RequestID: request.RequestID,
	})
	if err != nil {
		writeClassifiedStoreError(w, r, err,
			refuseSentinel(store.ErrTaskDependencyInvalid, http.StatusBadRequest, store.ErrTaskDependencyInvalid.Error()),
			refuseSentinel(store.ErrDependencyEdgeNotFound, http.StatusBadRequest, store.ErrDependencyEdgeNotFound.Error()),
			refuseSentinel(store.ErrTaskDependencyConflict, http.StatusBadRequest, "request_id was already used for different dependency removal inputs"),
			refuseSentinel(store.ErrNotFound, http.StatusBadRequest, "task not found"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) closeTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	if core.TaskTerminal(task.State) {
		http.Error(w, store.ErrTaskTerminal.Error(), http.StatusConflict)
		return
	}
	var request closeTaskRequest
	if err = decodeOptionalBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	// A reason is prose, counted in runes as a start-over reason is. A
	// reason_code stays a code of at most 64 bytes.
	reason := strings.TrimSpace(request.Reason)
	tooLong := utf8.RuneCountInString(reason) > 64
	if reason == "" {
		reason = strings.TrimSpace(request.ReasonCode)
		tooLong = len(reason) > 64
	}
	if reason == "" || tooLong {
		http.Error(w, "reason is required and must be at most 64 characters", http.StatusBadRequest)
		return
	}
	latest, hasJob, err := s.Store.GetLatestJob(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if !hasJob {
		latest = core.Job{}
	}
	intervention := core.Intervention{TaskID: id, JobID: latest.ID, Action: core.InterventionCancel, ReasonCode: reason, Comment: strings.TrimSpace(request.Comment)}
	if s.OnIntervention != nil {
		err = s.OnIntervention(r.Context(), task, latest, intervention)
	} else {
		_, err = taskops.New(s.Store).Cancel(r.Context(), intervention)
	}
	if err != nil {
		var invalid *core.ErrInvalidTransition
		if errors.Is(err, store.ErrTaskTerminal) || errors.As(err, &invalid) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeStoreFailure(w, r, err)
		return
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// The cancel committed before this read, so the answer is 200 and not 202
	// (req-task-lifecycle-and-queue REQ-8; component-task-lifecycle invariant 31).
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) reviewTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	if !reviewable(task.State) {
		http.Error(w, "task is not at a human gate", http.StatusConflict)
		return
	}
	_, planRevisionGate, err := dispatch.PendingPlanRevisionGate(r.Context(), s.Store, id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	var request reviewRequest
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	if !request.Action.Valid() || request.Action == core.InterventionCancel {
		http.Error(w, "invalid review action", http.StatusBadRequest)
		return
	}
	if task.State == core.TaskApproved && request.Action == core.InterventionApprove {
		// Approval never merges; name the separate merge act so a CLI caller
		// can find it (req-review-gates-evidence AC-1.1; component-runtime).
		http.Error(w, fmt.Sprintf("approved tasks must use the merge operation; run `conveyor task merge %s`", task.ID), http.StatusConflict)
		return
	}
	request.ReasonCode = strings.TrimSpace(request.ReasonCode)
	if request.ReasonCode == "" || len(request.ReasonCode) > 64 {
		http.Error(w, "reason_code is required and must be at most 64 characters", http.StatusBadRequest)
		return
	}
	request.Comment = strings.TrimSpace(request.Comment)
	if planRevisionGate {
		if request.Action == core.InterventionRedirect {
			request.Comment, err = core.NormalizeWorkOrderOperatorDirection(request.Comment)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		valid := request.Action == core.InterventionReject && request.ReasonCode == dispatch.PlanRevisionRejectedReasonCode
		valid = valid || request.Action == core.InterventionRedirect && request.ReasonCode == dispatch.PlanRevisionApprovedReasonCode
		valid = valid || request.Action == core.InterventionRedirect && request.ReasonCode == dispatch.PlanRevisionDeclinedReasonCode && request.Comment != ""
		if !valid {
			http.Error(w, "plan-revision decision requires an approved, declined-with-direction, or rejected reason code", http.StatusBadRequest)
			return
		}
	}
	checkoutCommand, checkoutAvailable, checkoutGuidance := s.checkoutState(id)
	if request.Action == core.InterventionPull && !checkoutAvailable {
		http.Error(w, checkoutGuidance, http.StatusConflict)
		return
	}
	latestJob, hasJob, err := s.Store.GetLatestJob(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	jobID := ""
	if hasJob {
		jobID = latestJob.ID
	}
	intervention := core.Intervention{
		TaskID: id, JobID: jobID, Action: request.Action,
		ReasonCode: request.ReasonCode, Comment: request.Comment,
	}
	if request.Action != core.InterventionPull {
		command := core.TaskInterventionApproveReview
		switch request.Action {
		case core.InterventionReject:
			command = core.TaskInterventionReject
		case core.InterventionRedirect:
			command = core.TaskInterventionRedirect
		}
		if _, err := core.TransitionTask(task.State, command); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	interventionRecorded := false
	if planRevisionGate {
		// A revision approval can make a plan order claimable immediately. Record
		// the decision before that transition so context assembly cannot observe
		// the re-entry order without its approving direction (REQ-2, AC-2.2).
		if err := s.Store.CreateIntervention(r.Context(), intervention); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeStoreFailure(w, r, err)
			return
		}
		interventionRecorded = true
	}
	if s.OnIntervention != nil {
		if err := s.OnIntervention(r.Context(), task, latestJob, intervention); err != nil {
			var invalidTransition *core.ErrInvalidTransition
			switch {
			case errors.As(err, &invalidTransition), errors.Is(err, dispatch.ErrReviewedHeadUnavailable):
				http.Error(w, err.Error(), http.StatusConflict)
			case github.ErrorCategory(err) != "" && !errors.Is(err, store.ErrBackendOperation):
				// A forge failure keeps its answer and its remedy.
				http.Error(w, err.Error(), http.StatusInternalServerError)
			default:
				writeStoreFailure(w, r, err)
			}
			return
		}
	}
	if !interventionRecorded {
		if err := s.Store.CreateIntervention(r.Context(), intervention); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeStoreFailure(w, r, err)
			return
		}
	}
	if s.OnIntervention == nil && request.Action == core.InterventionRedirect && s.OnCreate != nil {
		s.OnCreate(r.Context(), id)
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task":               updated,
		"checkout_command":   checkoutCommand,
		"checkout_available": checkoutAvailable,
		"checkout_guidance":  checkoutGuidance,
	})
}

func (s *Server) mergeTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	if task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval {
		s.mergeTaskAtGate(w, r, task)
		return
	}
	if task.State != core.TaskApproved && task.State != core.TaskMerged {
		http.Error(w, "task is not approved for merge", http.StatusConflict)
		return
	}
	if s.OnMerge == nil {
		http.Error(w, "merge operation is not configured", http.StatusNotImplemented)
		return
	}
	actor, err := store.RequireActor(r.Context())
	if err != nil {
		http.Error(w, "merge requires an authenticated operator", http.StatusForbidden)
		return
	}
	if err := s.OnMerge(r.Context(), task, actor); err != nil {
		writeMergeRouteError(w, r, err)
		return
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if updated.State != core.TaskMerged {
		http.Error(w, "merge was not confirmed by the forge", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// mergeTaskAtGate is the one-act operator merge at the merge_approval gate
// (DEC-64(1)-(6),(9)). The route's operate_gates capability check already
// ran; the act runs as the request credential's actor. A refusal names the
// readiness state and leaves the gate pending.
func (s *Server) mergeTaskAtGate(w http.ResponseWriter, r *http.Request, task core.Task) {
	if s.OnGateMerge == nil {
		http.Error(w, "merge operation is not configured", http.StatusNotImplemented)
		return
	}
	actor, err := store.RequireActor(r.Context())
	if err != nil {
		http.Error(w, "merge requires an authenticated operator", http.StatusForbidden)
		return
	}
	updated, err := s.OnGateMerge(r.Context(), task, actor)
	var notReady *dispatch.ErrGateMergeNotReady
	switch {
	case errors.As(err, &notReady):
		writeJSON(w, http.StatusConflict, map[string]string{"error": notReady.Error(), "readiness": notReady.State})
		return
	case errors.Is(err, dispatch.ErrGateMergeUnconfirmed):
		http.Error(w, "merge was not confirmed by the forge", http.StatusConflict)
		return
	case err != nil:
		writeMergeRouteError(w, r, err)
		return
	case updated.State != core.TaskMerged:
		http.Error(w, "merge was not confirmed by the forge", http.StatusConflict)
		return
	}
	s.mergeReadiness.forget(r.Context(), task.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) fixMergeConflict(w http.ResponseWriter, r *http.Request) {
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	if s.OnConflictFix == nil {
		http.Error(w, "merge conflict dispatch is not configured", http.StatusNotImplemented)
		return
	}
	order, err := s.OnConflictFix(r.Context(), task)
	if err != nil {
		writeMergeRouteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, order)
}

// writeMergeRouteError answers a merge act's failure: a refusal or a forge
// failure keeps 409 and its remedy, and a store failure answers the generic 500
// (DEC-105(1); req-delivery-and-forge AC-1.4, AC-1.8).
func writeMergeRouteError(w http.ResponseWriter, r *http.Request, err error) {
	if dispatch.MergeRouteRefusal(err) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeStoreFailure(w, r, err)
}

func reviewable(state core.TaskState) bool {
	return state == core.TaskAwaiting || state == core.TaskApproved
}

type createTaskReq struct {
	repositoryInstallAttempt int
	attachments              []store.ArtifactUpload
	multipartIntake          bool

	Body            string               `json:"body"`
	Repo            string               `json:"repo"`
	BaseBranch      string               `json:"base_branch"`
	Source          string               `json:"source"`
	Level           core.EscalationLevel `json:"level"`
	Hold            bool                 `json:"hold"` // deprecated input; intake refuses true after the idempotency lookup (DEC-74(3))
	SpecApproval    *bool                `json:"spec_approval,omitempty"`
	MergeApproval   *bool                `json:"merge_approval,omitempty"`
	Setup           string               `json:"setup,omitempty"`
	DependsOn       []string             `json:"depends_on,omitempty"`
	RequirementIDs  []string             `json:"requirement_ids,omitempty"`
	SystemDesignIDs []string             `json:"system_design_ids,omitempty"`
}

func (req *createTaskReq) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		if strings.EqualFold(name, "title") {
			return fmt.Errorf("title is generated and must not be supplied")
		}
		if strings.EqualFold(name, "mode") {
			return fmt.Errorf("mode is retired and must not be supplied")
		}
		switch strings.ToLower(name) {
		case "setup", "setup_contract", "policy_contract", "execution_settings", "routing", "harness", "model", "effort", "argv":
			return fmt.Errorf("%s is retired execution detail and must not be supplied", name)
		}
	}
	type request createTaskReq
	return json.Unmarshal(data, (*request)(req))
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		s.createTaskWithAttachments(w, r)
		return
	}
	var req createTaskReq
	if err := decodeBoundedJSON(w, r, taskCreateBodyLimit, &req); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	result, err := s.createTaskRecord(r.Context(), req, "", "api")
	if err != nil {
		writeTaskCreateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result.Task)
}

func (s *Server) createTaskWithAttachments(w http.ResponseWriter, r *http.Request) {
	limit := s.multipartTaskIntakeLimit
	if limit <= 0 {
		limit = maxMultipartTaskIntakeBytes
	}
	tooLarge := fmt.Sprintf("attachment task request exceeds the %d-byte multipart intake limit", limit)
	// The whole request is bounded before parsing; every refusal precedes
	// opening, validating, titling, or persisting an attachment
	// (component-artifacts "Atomic multipart intake").
	if r.ContentLength > limit {
		http.Error(w, tooLarge, http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(maxArtifactBytes); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		var overflow *http.MaxBytesError
		if errors.As(err, &overflow) {
			http.Error(w, tooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid attachment task form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	// The parser stops at the closing boundary. Drain any epilogue through
	// the same bounded reader so trailing bytes cannot exceed the limit
	// unnoticed.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var overflow *http.MaxBytesError
		if errors.As(err, &overflow) {
			http.Error(w, tooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid attachment task form: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req createTaskReq
	if err := json.Unmarshal([]byte(r.FormValue("task")), &req); err != nil {
		http.Error(w, "invalid task metadata: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.multipartIntake = true
	for _, header := range r.MultipartForm.File["attachments"] {
		upload, err := readTaskAttachment(header)
		if err != nil {
			http.Error(w, fmt.Sprintf("attachment %s: %v", safeFilename(header), err), http.StatusBadRequest)
			return
		}
		req.attachments = append(req.attachments, upload)
	}
	result, err := s.createTaskRecord(r.Context(), req, r.FormValue("idempotency_key"), "api")
	if err != nil {
		writeTaskCreateError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, result.Task)
}

func readTaskAttachment(header *multipart.FileHeader) (store.ArtifactUpload, error) {
	file, err := header.Open()
	if err != nil {
		return store.ArtifactUpload{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return store.ArtifactUpload{}, err
	}
	media, err := core.ValidateArtifactMedia(header.Header.Get("Content-Type"), content)
	if err != nil {
		return store.ArtifactUpload{}, err
	}
	return store.ArtifactUpload{Name: safeFilename(header), ContentType: media, Content: content}, nil
}

// defaultTaskListLimit bounds a GET /v1/tasks request that sends no limit, so
// an unparameterized read never loads the workspace's whole history
// (component-http-api, Task list read).
const defaultTaskListLimit = 100

// listTasks serves one page of task rows under the shared filter family. The
// paging headers are always set; without limit they state the default.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if query.Limit == 0 {
		query.Limit = defaultTaskListLimit
	}
	page, err := s.readTaskPage(r.Context(), query)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.writeTasks(w, r, page, query.Limit, query.Offset)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	t.Context, err = store.TaskContextForTask(r.Context(), s.Store, t.ID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// This read may serve a row stored raw, so its text passes the redactor
	// without rewriting the row. A stage's delivery doesn't take this path: it
	// receives the body as submitted (req-security-boundaries REQ-3, REQ-5;
	// req-intake-and-triage AC-1.2).
	redactor, err := s.mcpReadRedactor(r.Context())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	response, ok := s.taskResponse(w, r, t)
	if !ok {
		return
	}
	rendered, err := renderMCPReadItem(response, redactor)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rendered)
}

// listJobs serves one store page of a task's jobs (DEC-105(4)).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	listed, ok := readListPage(w, r, defaultHistoryPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListJobPage(r.Context(), chi.URLParam(r, "id"), listed.query())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, page.Jobs)
}

// listEvents serves one store page of a task's ledger under the request's
// workspace scope (DEC-105(4); component-http-api invariant 13). The task event
// window route keeps its own cursor traversal
// (req-task-lifecycle-and-queue AC-10.1).
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	listed, ok := readListPage(w, r, defaultHistoryPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListEventPage(r.Context(), chi.URLParam(r, "id"), listed.query())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// This read may serve a row stored raw, so its text passes the redactor
	// without rewriting the row (req-security-boundaries REQ-5). The paging
	// headers go out only with the rows, so a failed read states no page.
	redactor, err := s.mcpReadRedactor(r.Context())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if err = s.annotateEventActorOwners(r.Context(), page.Events); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	for index, event := range page.Events {
		if page.Events[index], err = redactEventForRead(event, redactor); err != nil {
			writeStoreFailure(w, r, fmt.Errorf("event %d: %w", event.ID, err))
			return
		}
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, page.Events)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	// mode=notify is the dashboard's fingerprint stream; without mode the
	// legacy replay is unchanged (task 261015-3721c6; component-http-api).
	notify := false
	if values, present := r.URL.Query()["mode"]; present {
		if len(values) != 1 || values[0] != "notify" {
			http.Error(w, "mode must be notify or omitted", http.StatusBadRequest)
			return
		}
		notify = true
	}
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// Subscribe before the first read, so a commit between that read and the
	// first wait still wakes the stream (component-http-api, "When a task
	// stream reads").
	wake := s.openTaskWake(taskStreamWorkspace(r.Context(), task), chi.URLParam(r, "id"), notify)
	defer wake.close()
	if notify {
		s.streamTaskEventNotifications(w, flusher, r, chi.URLParam(r, "id"), wake)
		return
	}
	stream := newTaskEventStream(s.Store, chi.URLParam(r, "id"), s.mcpReadRedactor)
	stream.annotate = s.annotateEventActorOwners
	for {
		if err := stream.poll(r.Context(), w); err != nil {
			if errors.Is(err, errTaskEventStreamRead) {
				_, _ = w.Write([]byte("event: error\ndata: {}\n\n"))
				flusher.Flush()
			}
			return
		}
		flusher.Flush()
		if _, ok := wake.wait(r.Context(), w, flusher); !ok {
			return
		}
	}
}

func (s *Server) listInterventions(w http.ResponseWriter, r *http.Request) {
	interventions, err := s.Store.ListInterventions(r.Context(), chi.URLParam(r, "id"))
	if err == nil {
		err = s.annotateInterventionActorOwners(r.Context(), interventions)
	}
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, interventions)
}

func (s *Server) getLatestSpec(w http.ResponseWriter, r *http.Request) {
	spec, ok, err := s.Store.GetLatestSpecVersion(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "spec not found", http.StatusNotFound)
		return
	}
	if task, taskErr := s.Store.GetTask(r.Context(), chi.URLParam(r, "id")); taskErr == nil {
		spec.MaterializedChildren = materializedChildrenForSpec(task.Children, spec.Version)
	}
	writeJSON(w, http.StatusOK, spec)
}

func materializedChildrenForSpec(children []core.TaskRelation, version int) []core.TaskRelation {
	var result []core.TaskRelation
	for _, child := range children {
		if child.OriginSpecVersion == version {
			result = append(result, child)
		}
	}
	return result
}

type reviewItem struct {
	Task                      core.Task                             `json:"task"`
	Jobs                      []core.Job                            `json:"jobs"`
	Events                    []core.Event                          `json:"events"`
	Interventions             []core.Intervention                   `json:"interventions"`
	CheckoutCommand           string                                `json:"checkout_command,omitempty"`
	CheckoutAvailable         bool                                  `json:"checkout_available"`
	CheckoutGuidance          string                                `json:"checkout_guidance"`
	NeedsAttention            bool                                  `json:"needs_attention"`
	NextAction                *nextaction.Compact                   `json:"next_action,omitempty"`
	AtMergeGate               bool                                  `json:"at_merge_gate"`
	ForgeFailure              *store.ForgeFailure                   `json:"forge_failure,omitempty"`
	Spec                      *core.SpecVersion                     `json:"spec,omitempty"`
	WorkOrders                []workOrderActivityView               `json:"work_orders"`
	ReviewDiagnostics         []store.ReviewVerdictDiagnostic       `json:"review_diagnostics,omitempty"`
	ReviewRecovery            *store.ReviewRecoveryState            `json:"review_recovery,omitempty"`
	InterruptedReviewRecovery *store.InterruptedReviewRecoveryState `json:"interrupted_review_recovery,omitempty"`
	Stalled                   *store.StalledState                   `json:"stalled,omitempty"`
	MergeReadiness            *dispatch.MergeReadiness              `json:"merge_readiness,omitempty"`
	Attachments               []core.Artifact                       `json:"attachments"`
	VerificationEvidence      []core.Artifact                       `json:"verification_evidence"`
}

type activityItem struct {
	Task                      activityTask                          `json:"task"`
	LatestStage               core.Stage                            `json:"latest_stage,omitempty"`
	LastEventAt               time.Time                             `json:"last_event_at"`
	NeedsAttention            bool                                  `json:"needs_attention"`
	NextAction                *nextaction.Compact                   `json:"next_action,omitempty"`
	ForgeFailure              *store.ForgeFailure                   `json:"forge_failure,omitempty"`
	ReviewDiagnostics         []store.ReviewVerdictDiagnostic       `json:"review_diagnostics,omitempty"`
	ReviewRecovery            *store.ReviewRecoveryState            `json:"review_recovery,omitempty"`
	InterruptedReviewRecovery *store.InterruptedReviewRecoveryState `json:"interrupted_review_recovery,omitempty"`
	Stalled                   *store.StalledState                   `json:"stalled,omitempty"`
}

// activityTask is the bounded board/navigation projection. Task bodies,
// execution policy, forge state, and other detail-only fields stay on the
// per-task activity endpoint (component-http-api).
type activityTask struct {
	Supersedes      string              `json:"supersedes,omitempty"`
	SupersededBy    string              `json:"superseded_by,omitempty"`
	ID              string              `json:"id"`
	Title           string              `json:"title"`
	Repo            string              `json:"repo"`
	State           core.TaskState      `json:"state"`
	NextStage       core.Stage          `json:"next_stage,omitempty"`
	Assignee        *core.TaskAssignee  `json:"assignee,omitempty"`
	ReviewedHeadSHA string              `json:"reviewed_head_sha,omitempty"`
	Dependencies    []core.TaskRelation `json:"dependencies,omitempty"`
	BlockingTaskIDs []string            `json:"blocking_task_ids,omitempty"`
	Children        []core.TaskRelation `json:"children,omitempty"`
	Context         core.TaskContext    `json:"context,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
}

func summarizeActivityTask(task core.Task) activityTask {
	return activityTask{
		Supersedes: task.Supersedes, SupersededBy: task.SupersededBy,
		ID: task.ID, Title: task.Title, Repo: task.Repo, State: task.State,
		NextStage: task.NextStage, Assignee: task.Assignee,
		ReviewedHeadSHA: task.ReviewedHeadSHA, Dependencies: task.Dependencies,
		BlockingTaskIDs: task.BlockingTaskIDs, Children: task.Children,
		Context: task.Context, CreatedAt: task.CreatedAt,
	}
}

// The review inbox is the workspace's whole outstanding queue, so it reads the
// unfiltered workspace: an operator narrowing the board must not also narrow
// what is waiting on them.
func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	s.listActivityFiltered(w, r, store.TaskFilter{}, true)
}

// The board applies the shared Tasks/Board filter family through the same store
// predicate the Tasks list uses, so the two surfaces cannot narrow differently
// (AC-2.4).
func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Activity is the Board's recent projection, not the historical archive.
	// Keep the array response compatible while bounding every default read;
	// callers can walk older history through the additive page parameters.
	if query.Limit == 0 {
		query.Limit = 100
	}
	s.listActivityPage(w, r, query)
}

// listCallerAttentionTasks is a bounded personal inbox. The subject comes
// only from the authenticated credential; there is deliberately no assignee
// parameter that could turn another member's attention into an existence
// oracle (DEC-19, req-260810-23b69f REQ-3). The route reads no filter, so it
// refuses every parameter but workspace_id and the page parameters instead of
// ignoring it (component-next-action invariant 24; DEC-105(2)).
func (s *Server) listCallerAttentionTasks(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if name != "workspace_id" && name != "limit" && name != "offset" {
			http.Error(w, "attention takes its subject from the credential and accepts no filter: "+name, http.StatusBadRequest)
			return
		}
	}
	listed, ok := readListPage(w, r, defaultTaskListLimit)
	if !ok {
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.OwnerUserID == "" {
		http.Error(w, "authenticated user is required", http.StatusUnauthorized)
		return
	}
	page, err := s.Store.ListCallerAttentionTaskPage(r.Context(), store.CallerAttentionQuery{
		UserID: credential.OwnerUserID, Limit: listed.Limit, Offset: listed.Offset,
	})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	listed.writeHeaders(w, page.Total)
	s.writeActivityItems(w, r, page.Tasks, false, nil)
}

// readTaskPage reads one page of the shared task filter. The no-auth
// in-memory server used by embedders predates explicit workspace resolution.
// Preserve that singleton compatibility path while production requests
// continue through the workspace-scoped store page.
func (s *Server) readTaskPage(ctx context.Context, query store.TaskOperationsQuery) (store.TaskPage, error) {
	compatibilityStore, supportsUnscopedActivity := s.Store.(store.UnscopedActivityStore)
	if s.Workspace == "" && s.Workspaces == nil && supportsUnscopedActivity && compatibilityStore.SupportsUnscopedActivity() {
		tasks, err := s.Store.ListTasksFiltered(ctx, query.TaskFilter)
		if err != nil {
			return store.TaskPage{}, err
		}
		total := len(tasks)
		start := min(query.Offset, total)
		end := min(start+query.Limit, total)
		return store.TaskPage{Tasks: tasks[start:end], Total: total}, nil
	}
	return s.Store.ListTaskPage(ctx, query)
}

// listActivityPage answers an unchanged Board page with 304 before it reads
// any marker, order, proposal, or next action (component-http-api, Activity
// conditional admission). The validation read precedes the page read, so a
// published tag never certifies a page read from an earlier state.
func (s *Server) listActivityPage(w http.ResponseWriter, r *http.Request, query store.TaskOperationsQuery) {
	since, err := decodeActivityCursor(r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := activityClock().UTC().Truncate(time.Microsecond)
	opts, err := s.nextActionOptions(r)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	opts.Now = now
	validation, err := s.Store.ReadActivityValidation(r.Context(), now)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	conditional := &activityConditional{now: now, since: since, opts: opts, scope: activityValidatorScope(r.Context(), query, opts.Capabilities), validation: validation}
	tag, admitted := conditional.admits(r.Header.Get("If-None-Match"))
	page, err := s.readTaskPage(r.Context(), query)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(query.Limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(query.Offset))
	if admitted {
		w.Header().Set("X-Conveyor-Cursor", tag.cursor)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", tag.raw)
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.writeActivityItems(w, r, page.Tasks, false, conditional)
}

func (s *Server) listActivityFiltered(w http.ResponseWriter, r *http.Request, filter store.TaskFilter, reviewsOnly bool) {
	tasks, err := s.Store.ListTasksFiltered(r.Context(), filter)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.writeActivityItems(w, r, tasks, reviewsOnly, nil)
}

// writeActivityItems projects the activity rows from one marker and order
// read. A Board read passes its conditional state, whose instant, cursor, and
// options it reuses; Reviews and Attention pass nil and keep the full-page
// ETag (component-http-api, Activity conditional admission).
func (s *Server) writeActivityItems(w http.ResponseWriter, r *http.Request, tasks []core.Task, reviewsOnly bool, conditional *activityConditional) {
	taskIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	var since activityCursor
	var opts store.NextActionOptions
	var err error
	if conditional != nil {
		since, opts = conditional.since, conditional.opts
	} else {
		if since, err = decodeActivityCursor(r.URL.Query().Get("since")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if opts, err = s.nextActionOptions(r); err != nil {
			log.Printf("handle API request: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	// One order selection feeds the markers and the next action
	// (component-persistence, Board activity validation reads).
	inputs, err := s.Store.ReadActivityInputs(r.Context(), taskIDs, opts.Now)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markers, orders := inputs.Markers, inputs.Orders
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	opts.MarkersLoaded, opts.Markers = true, markers
	opts.OrdersLoaded, opts.Orders = true, orders
	projected, err := store.ReadNextActions(r.Context(), s.Store, tasks, opts)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	items := make([]activityItem, 0, len(tasks))
	fullItems := make([]activityItem, 0, len(tasks))
	for _, task := range tasks {
		// Blueprint anchors are intent artifacts, not claimable work, so they
		// leave the stage-grouped board and its counts for the Blueprints
		// surface (component-http-api). The review inbox projection keeps them: an
		// anchor at its spec gate has not materialized children yet, so it is
		// not classified here and its approval card is untouched.
		if !reviewsOnly && core.BlueprintAnchor(task) {
			continue
		}
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		if reviewsOnly && !reviewable(task.State) && marker.ForgeFailure == nil && marker.ReviewRecovery == nil && marker.InterruptedReviewRecovery == nil && marker.Stalled == nil && !marker.UserChangesRequested {
			continue
		}
		compact := nextaction.CompactFrom(projected[task.ID])
		item := activityItem{
			Task: summarizeActivityTask(task), LatestStage: marker.LatestStage, LastEventAt: marker.LastEventAt,
			NeedsAttention:            projected[task.ID].Actor == nextaction.ActorOperator,
			NextAction:                &compact,
			ForgeFailure:              marker.ForgeFailure,
			ReviewDiagnostics:         marker.ReviewDiagnostics,
			ReviewRecovery:            marker.ReviewRecovery,
			InterruptedReviewRecovery: marker.InterruptedReviewRecovery,
			Stalled:                   marker.Stalled,
		}
		fullItems = append(fullItems, item)
		if !since.delta || store.ActivityMarkerAfter(marker, since.frontier) {
			items = append(items, item)
		}
	}
	// The frontier is the page's maximum (at, id) tuple, so a later event with
	// a lower ID still enters the next delta (component-http-api;
	// component-persistence).
	cursor := encodeActivityCursor(store.ActivityFrontier(markers))
	w.Header().Set("X-Conveyor-Cursor", cursor)
	if conditional == nil {
		writeConditionalJSON(w, r, items, fullItems)
		return
	}
	validUntil := conditional.validUntil(inputs.ValidUntil, fullItems)
	// A write that landed between the two validation reads may be half in
	// this body, so its tag must never admit a 304. No retry is attempted.
	after, err := s.Store.ReadActivityValidation(r.Context(), conditional.now)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if after != conditional.validation {
		validUntil = conditional.now
	}
	writeJSONWithETag(w, r, items, conditional.etag(validUntil, cursor))
}

// activityConditional is one Board request's validator state: the captured
// instant, the decoded cursor and next-action options, the bound scope, and
// the validation read that precedes the page read.
type activityConditional struct {
	now        time.Time
	since      activityCursor
	opts       store.NextActionOptions
	scope      []byte
	validation store.ActivityValidation
}

// activityTag is a parsed Board ETag: "a1.<valid_until micros>.<cursor>.<hex>".
type activityTag struct {
	raw        string
	validUntil time.Time
	cursor     string
	validator  string
}

const activityTagVersion = "a1"

// activityClock is the Board request clock. HTTP tests replace it to cross a
// minute boundary and deadlines inside one minute without sleeping.
var activityClock = time.Now

// activityValidatorScope binds a tag to its workspace, caller, capabilities,
// and parsed query, so a tag from another page, workspace, or capability set
// never admits a 304. The cursor is deliberately not bound: a full-page tag
// stays reusable with the returned cursor.
func activityValidatorScope(ctx context.Context, query store.TaskOperationsQuery, capabilities map[core.Capability]bool) []byte {
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	credential, _ := store.CredentialFromContext(ctx)
	granted := make([]string, 0, len(nextActionCapabilities))
	for _, capability := range nextActionCapabilities {
		if capabilities[capability] {
			granted = append(granted, string(capability))
		}
	}
	scope, _ := json.Marshal(struct {
		Version      string                    `json:"version"`
		Workspace    string                    `json:"workspace"`
		User         string                    `json:"user"`
		Capabilities []string                  `json:"capabilities"`
		Query        store.TaskOperationsQuery `json:"query"`
	}{"activity-v1", workspaceID, credential.OwnerUserID, granted, query})
	return scope
}

// validator hashes the scope, the validation read, the request's UTC minute,
// the tag's valid_until, and the response cursor.
func (c *activityConditional) validator(validUntil time.Time, cursor string) string {
	hash := sha256.New()
	field := func(value []byte) {
		hash.Write(strconv.AppendInt(nil, int64(len(value)), 10))
		hash.Write([]byte{':'})
		hash.Write(value)
	}
	number := func(value int64) { field(strconv.AppendInt(nil, value, 10)) }
	field(c.scope)
	number(c.validation.EventCount)
	number(c.validation.EventMaxID)
	field(c.validation.Signature[:])
	number(c.validation.PassedDeadlines)
	number(c.now.Truncate(time.Minute).Unix())
	number(validUntil.UnixMicro())
	field([]byte(cursor))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (c *activityConditional) etag(validUntil time.Time, cursor string) string {
	return `"` + activityTagVersion + "." + strconv.FormatInt(validUntil.UnixMicro(), 10) + "." + cursor + "." + c.validator(validUntil, cursor) + `"`
}

// admits reports whether If-None-Match carries this request's Board tag: the
// form parses, the request instant is before its valid_until, the request
// cursor is compatible with the tag, and the validator recomputed from the
// current state matches.
func (c *activityConditional) admits(header string) (activityTag, bool) {
	inner, ok := strings.CutPrefix(header, `"`)
	if !ok {
		return activityTag{}, false
	}
	if inner, ok = strings.CutSuffix(inner, `"`); !ok {
		return activityTag{}, false
	}
	fields := strings.Split(inner, ".")
	if len(fields) != 4 || fields[0] != activityTagVersion {
		return activityTag{}, false
	}
	micros, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || micros <= 0 {
		return activityTag{}, false
	}
	tag := activityTag{raw: header, validUntil: time.UnixMicro(micros).UTC(), cursor: fields[2], validator: fields[3]}
	if !c.cursorCompatible(tag.cursor) || !c.now.Before(tag.validUntil) || c.validator(tag.validUntil, tag.cursor) != tag.validator {
		return activityTag{}, false
	}
	return tag, true
}

// cursorCompatible reports whether the request's decoded cursor asks for a
// body the tag certifies while the state is unchanged: a full read (an empty
// since, a zero v2 frontier, or a legacy v1 cursor), or a delta from exactly
// the frontier the tag's response returned. Any other valid frontier, older,
// newer, or in the future, takes the ordinary 200 path (component-http-api,
// Activity conditional admission).
func (c *activityConditional) cursorCompatible(tagCursor string) bool {
	return !c.since.delta || encodeActivityCursor(c.since.frontier) == tagCursor
}

// validUntil is the earliest of the next UTC minute boundary, the review
// lease horizon, and every item's compact deadline after the instant, which
// covers a merge-conflict retry time that changes a retrying summary.
func (c *activityConditional) validUntil(reviewLease time.Time, items []activityItem) time.Time {
	until := c.now.Truncate(time.Minute).Add(time.Minute)
	if reviewLease.After(c.now) && reviewLease.Before(until) {
		until = reviewLease
	}
	for _, item := range items {
		if item.NextAction == nil || item.NextAction.Deadline == nil {
			continue
		}
		if deadline := *item.NextAction.Deadline; deadline.After(c.now) && deadline.Before(until) {
			until = deadline
		}
	}
	return until
}

// activityCursor is a decoded `since` value. A zero value, an empty cursor, a
// zero frontier, or a legacy v1 numeric cursor all read the full page; only a
// v2 cursor with a nonzero frontier selects a delta.
type activityCursor struct {
	delta    bool
	frontier store.TaskEventPosition
}

// encodeActivityCursor writes the v2 cursor: UTC Unix microseconds and the
// decimal event ID of the frontier tuple. The zero frontier encodes as 0:0.
func encodeActivityCursor(frontier store.TaskEventPosition) string {
	var micros int64
	if !frontier.At.IsZero() {
		micros = frontier.At.UTC().UnixMicro()
	}
	return base64.RawURLEncoding.EncodeToString([]byte("v2:" + strconv.FormatInt(micros, 10) + ":" + strconv.FormatInt(frontier.ID, 10)))
}

// decodeActivityCursor accepts v2 and the legacy v1 numeric cursor. A v1
// cursor cannot locate a time, so it reads the full page once and the response
// carries a v2 cursor. Malformed input, a negative time or ID, and an unknown
// version are refused.
func decodeActivityCursor(value string) (activityCursor, error) {
	if value == "" {
		return activityCursor{}, nil
	}
	invalid := fmt.Errorf("invalid activity cursor")
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return activityCursor{}, invalid
	}
	text := string(raw)
	switch {
	case strings.HasPrefix(text, "v1:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(text, "v1:"), 10, 64)
		if err != nil || id < 0 {
			return activityCursor{}, invalid
		}
		return activityCursor{}, nil
	case strings.HasPrefix(text, "v2:"):
		fields := strings.Split(strings.TrimPrefix(text, "v2:"), ":")
		if len(fields) != 2 {
			return activityCursor{}, invalid
		}
		micros, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || micros < 0 {
			return activityCursor{}, invalid
		}
		id, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || id < 0 {
			return activityCursor{}, invalid
		}
		if micros == 0 && id == 0 {
			return activityCursor{}, nil
		}
		frontier := store.TaskEventPosition{ID: id}
		if micros > 0 {
			frontier.At = time.UnixMicro(micros).UTC()
		}
		return activityCursor{delta: true, frontier: frontier}, nil
	default:
		return activityCursor{}, invalid
	}
}

func writeConditionalJSON(w http.ResponseWriter, r *http.Request, v, etagValue any) {
	etagBody, err := json.Marshal(etagValue)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(etagBody)
	etag := fmt.Sprintf("\"%x\"", sum)
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONWithETag(w, r, v, etag)
}

// writeJSONWithETag writes v with a precomputed strong ETag and negotiates
// gzip. It never answers 304: the Board's conditional admission is its only
// 304, so a refused tag always receives the body (component-http-api,
// Activity conditional admission).
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, v any, etag string) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = io.Copy(zw, bytes.NewReader(body))
		_ = zw.Close()
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// Plan status is the four durable outcomes AC-1.4 names. Each is read off the
// persisted plan version and the audited task-state command that parked or
// resumed it; none is inferred when that authority is absent.
const (
	taskPlanNone        = "none"
	taskPlanPendingGate = "pending_gate"
	taskPlanApproved    = "approved"
	taskPlanRedirected  = "redirected"
)

// taskPlanStatus renders a task's execution-plan state for the Tasks view. A
// task with no plan version carries state "none" and no version, so absence is
// represented rather than filled in (AC-1.4).
type taskPlanStatus struct {
	State   string `json:"state"`
	Version int    `json:"version,omitempty"`
	// Legacy marks an unapproved pre-Phase-8.3 spec version captured by
	// migration 075, so the row reads as the historical record it is
	// by construction.
	Legacy bool `json:"legacy,omitempty"`
}

// taskChildRollup summarizes a task's materialized children. It exists only
// when children exist, so a childless task renders as having none rather than
// as a rollup of zeroes (AC-1.2).
type taskChildRollup struct {
	Total  int `json:"total"`
	Merged int `json:"merged"`
	Closed int `json:"closed"`
	Open   int `json:"open"`
}

// taskOperationsItem is the list-first Tasks view's row (component-http-api).
// Every field projects durable task, relationship, context, or plan authority;
// the view stores nothing and re-derives nothing of its own. No priority or
// declared-phase field appears here; assignee is durable task authority.
type taskOperationsItem struct {
	Task        core.Task  `json:"task"`
	LatestStage core.Stage `json:"latest_stage,omitempty"`
	LastEventAt time.Time  `json:"last_event_at"`
	// Stalled says why a row cannot move on its own, so staleness is legible
	// from the task-level surface. The authority is the same derived stalled
	// state the board and task detail read (component-http-api); only the
	// fields a list row states travel with it.
	Stalled              *taskStalledSummary `json:"stalled,omitempty"`
	NeedsAttention       bool                `json:"needs_attention"`
	NextAction           *nextaction.Compact `json:"next_action,omitempty"`
	UnsatisfiableTaskIDs []string            `json:"unsatisfiable_task_ids,omitempty"`
	ChildRollup          *taskChildRollup    `json:"child_rollup,omitempty"`
	Plan                 taskPlanStatus      `json:"plan"`
}

// taskStalledSummary is the list-scoped view of store.StalledState. The detail
// endpoints carry the whole state because they render the work order; a list
// row renders a sentence, and store.StalledState embeds a core.WorkOrder whose
// governance snapshot carries entire System Design documents — content that has
// no business repeating once per row of a whole-workspace list.
type taskStalledSummary struct {
	Needed      bool   `json:"needed"`
	Reason      string `json:"reason"`
	LastFailure string `json:"last_failure,omitempty"`
}

func taskStalledSummaryFor(stalled *store.StalledState) *taskStalledSummary {
	if stalled == nil {
		return nil
	}
	return &taskStalledSummary{Needed: stalled.Needed, Reason: stalled.Reason, LastFailure: stalled.LastFailure}
}

// listTaskOperations serves the list-first Tasks view (component-http-api).
// Dependencies, blocking edges, and children come from a bounded store page;
// attached context and plan status fold only the event kinds and latest plan
// records the projection batches for those returned tasks. The historical
// blueprint read path is not a scale precedent for this daily surface. Only
// the shared filter family, limit, and offset are read, so any other query
// parameter, including the retired group selector, is ignored. A request
// without limit reads the default page (DEC-105(4)).
func (s *Server) listTaskOperations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query, _, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if query.Limit == 0 {
		query.Limit = defaultTaskListLimit
	}
	page, err := s.Store.ListTaskOperations(ctx, query)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	listPage{Limit: query.Limit, Offset: query.Offset, Paged: true}.writeHeaders(w, page.Total)
	taskIDs := make([]string, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	markers, err := s.Store.ListActivityMarkersForTasks(ctx, taskIDs)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts.MarkersLoaded, opts.Markers = true, markers
	projected, err := store.ReadNextActions(ctx, s.Store, page.Tasks, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// (component-task-lifecycle); the view reads it, and reads the unsatisfiable edges the
	// task record itself does not carry.
	blockers, err := s.Store.ListDependencyBlockers(ctx, taskIDs)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	items, err := s.taskOperationsItems(ctx, page.Tasks, page.Events, page.Plans, markerByTask, projected, blockers)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// taskOperationsItems assembles task-operations rows. A row carries no pull
// request; the bounded detail's summaries.pull_request and the inbox item
// state it (component-http-api, Activity and attention reads).
func (s *Server) taskOperationsItems(ctx context.Context, tasks []core.Task, events map[string][]core.Event, plans map[string]core.SpecVersion, markerByTask map[string]store.ActivityMarker, projected map[string]nextaction.NextAction, blockers map[string]store.DependencyBlockers) ([]taskOperationsItem, error) {
	// Attached documents repeat across rows; memoizing only the document reads
	// keeps the authoritative fold in store.TaskContextFromEvents.
	documents := &taskContextDocumentCache{Store: s.Store, requirements: map[string]core.Requirement{}, designs: map[string]core.SystemDesign{}}
	items := make([]taskOperationsItem, 0, len(tasks))
	for _, task := range tasks {
		taskEvents := events[task.ID]
		var err error
		task.Context, err = store.TaskContextFromEvents(ctx, documents, taskEvents)
		if err != nil {
			return nil, err
		}
		latest, hasPlan := plans[task.ID]
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		compact := nextaction.CompactFrom(projected[task.ID])
		items = append(items, taskOperationsItem{
			Task:                 task,
			LatestStage:          marker.LatestStage,
			LastEventAt:          marker.LastEventAt,
			Stalled:              taskStalledSummaryFor(marker.Stalled),
			NeedsAttention:       projected[task.ID].Actor == nextaction.ActorOperator,
			NextAction:           &compact,
			UnsatisfiableTaskIDs: blockers[task.ID].UnsatisfiableTaskIDs,
			ChildRollup:          taskChildRollupFor(task.Children),
			Plan:                 taskPlanStatusFor(task, latest, hasPlan, taskEvents),
		})
	}
	return items, nil
}

// parseTaskFilter reads the one filter family the Tasks list and the Board both
// send (AC-2.4). Both routes parse it here, so a parameter cannot mean one
// thing on one surface and another on the other. Unparseable input is rejected
// rather than dropped: a silently ignored filter reads as a workspace that
// contains the wrong rows.
func parseTaskFilter(values url.Values) (store.TaskFilter, error) {
	// Every list member repeats its parameter — `state=a&state=b` — which keeps
	// a single-valued caller byte-compatible with the historical form.
	repositories := parseTaskFilterList(values["repository"])
	if len(repositories) == 0 {
		repositories = parseTaskFilterList(values["repo"])
	}
	filter := store.TaskFilter{
		Repositories:         repositories,
		Assignee:             strings.TrimSpace(values.Get("assignee")),
		Query:                strings.TrimSpace(values.Get("q")),
		ServesRequirementIDs: parseTaskFilterList(values["serves_requirement"]),
		GoverningDesignIDs:   parseTaskFilterList(values["governing_design"]),
	}
	for _, state := range parseTaskFilterList(values["state"]) {
		candidate := core.TaskState(state)
		valid := false
		for _, known := range core.TaskStates() {
			valid = valid || candidate == known
		}
		if !valid {
			return filter, fmt.Errorf("invalid task state %q", state)
		}
		filter.States = append(filter.States, candidate)
	}
	// Updated bounds select on last activity — the row's "Updated" instant —
	// and Created bounds on the persisted creation instant (AC-2.4). They are
	// separate predicates: neither is an alias for the other, each is validated
	// on its own, and a task must satisfy both when both are sent.
	var err error
	if filter.UpdatedFrom, err = parseTaskFilterInstant("updated_from", values.Get("updated_from")); err != nil {
		return filter, err
	}
	if filter.UpdatedTo, err = parseTaskFilterInstant("updated_to", values.Get("updated_to")); err != nil {
		return filter, err
	}
	if filter.CreatedFrom, err = parseTaskFilterInstant("created_from", values.Get("created_from")); err != nil {
		return filter, err
	}
	if filter.CreatedTo, err = parseTaskFilterInstant("created_to", values.Get("created_to")); err != nil {
		return filter, err
	}
	return filter, filter.Validate()
}

// parseTaskFilterList trims a repeated parameter's values and drops the empty
// ones, so `state=` narrows nothing rather than matching nothing.
func parseTaskFilterList(raw []string) []string {
	var out []string
	for _, value := range raw {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// parseTaskFilterInstant takes an absolute RFC 3339 instant. The bound is
// resolved in the browser, where the operator's own day boundaries live, so the
// server never has to guess a timezone for "the last month".
func parseTaskFilterInstant(name, raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 instant: %w", name, err)
	}
	return value, nil
}

func parseTaskOperationsQuery(r *http.Request) (store.TaskOperationsQuery, bool, error) {
	values := r.URL.Query()
	filter, err := parseTaskFilter(values)
	if err != nil {
		return store.TaskOperationsQuery{}, false, err
	}
	query := store.TaskOperationsQuery{TaskFilter: filter}
	page, err := parseListPage(values, 0)
	if err != nil {
		return query, false, err
	}
	query.Limit, query.Offset = page.Limit, page.Offset
	return query, page.Paged, nil
}

// taskContextDocumentCache memoizes the requirement and design reads that
// resolve attached-context titles and versions across one list pass. Only
// those two lookups are cached: every other call reaches the real store, so
// the fold itself stays store.TaskContextFromEvents.
type taskContextDocumentCache struct {
	store.Store
	requirements map[string]core.Requirement
	designs      map[string]core.SystemDesign
}

func (c *taskContextDocumentCache) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	if cached, ok := c.requirements[id]; ok {
		return cached, nil
	}
	document, err := c.Store.GetRequirement(ctx, id)
	if err != nil {
		return document, err
	}
	c.requirements[id] = document
	return document, nil
}

func (c *taskContextDocumentCache) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	if cached, ok := c.designs[id]; ok {
		return cached, nil
	}
	document, err := c.Store.GetSystemDesign(ctx, id)
	if err != nil {
		return document, err
	}
	c.designs[id] = document
	return document, nil
}

func taskChildRollupFor(children []core.TaskRelation) *taskChildRollup {
	if len(children) == 0 {
		return nil
	}
	rollup := taskChildRollup{Total: len(children)}
	for _, child := range children {
		switch child.State {
		case core.TaskMerged:
			rollup.Merged++
		case core.TaskClosed:
			rollup.Closed++
		default:
			rollup.Open++
		}
	}
	return &rollup
}

// taskPlanStatusFor derives plan status from durable authority alone. Approval
// only ever lands on the newest version, so an unapproved latest version is
// still inside the gate loop; the audited task.state_changed command then
// separates a plan waiting at its gate from one the operator redirected, the
// same recognition the dispatcher's own spec-gate check performs
// (component-http-api).
func taskPlanStatusFor(task core.Task, latest core.SpecVersion, hasPlan bool, events []core.Event) taskPlanStatus {
	if !hasPlan {
		return taskPlanStatus{State: taskPlanNone}
	}
	status := taskPlanStatus{State: taskPlanApproved, Version: latest.Version, Legacy: latest.LegacyGate}
	if latest.Approved {
		return status
	}
	status.State = taskPlanPendingGate
	if task.State == core.TaskAwaiting && task.GateKind != "" {
		return status
	}
	// A plan version is created at its gate, so pending is the state it
	// entered; a later redirect is the only audited command that moves it.
	status.State = taskPlanPendingGate
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "task.state_changed" {
			continue
		}
		var transition struct {
			Command core.TaskCommand `json:"command"`
		}
		if json.Unmarshal(events[i].Payload, &transition) != nil {
			continue
		}
		switch transition.Command {
		case core.TaskGateSpec:
			return status
		case core.TaskInterventionRedirect:
			status.State = taskPlanRedirected
			return status
		}
	}
	return status
}

func (s *Server) getTaskActivity(w http.ResponseWriter, r *http.Request) {
	// view=bounded is the dashboard's detail read; without view the complete
	// representation is unchanged for every existing caller (task
	// 261015-3721c6; component-http-api "Bounded detail view").
	bounded := false
	if values, present := r.URL.Query()["view"]; present {
		if len(values) != 1 || values[0] != "bounded" {
			http.Error(w, "view must be bounded or omitted", http.StatusBadRequest)
			return
		}
		bounded = true
	}
	id := chi.URLParam(r, "id")
	// The task, its events, and its orders are read once per request and
	// every fold takes these copies (component-http-api, Request-scoped
	// detail reads).
	readStart := s.mergeReadiness.now()
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	var mergeReadiness *dispatch.MergeReadiness
	// Readiness resolves before any other read, so every later read observes
	// whatever the hook wrote. A merge_approval gate carries the
	// side-effect-free peek, which offers the one-act merge only at MERGEABLE
	// (DEC-64(3); DEC-61).
	approved := task.State == core.TaskApproved
	gateMerge := task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval
	if (approved || gateMerge) && s.OnMergeReadiness != nil {
		readiness, fresh, readinessErr := s.mergeReadiness.readObserved(r.Context(), task, s.OnMergeReadiness, readStart)
		switch {
		case readinessErr != nil && approved:
			// The merge action must never render without a successful
			// readiness read (component-http-api).
			if !dispatch.MergeRouteRefusal(readinessErr) {
				writeStoreFailure(w, r, readinessErr)
				return
			}
			http.Error(w, fmt.Sprintf("resolve merge readiness: %v", readinessErr), http.StatusServiceUnavailable)
			return
		case readinessErr != nil:
			// A failed gate peek omits merge_readiness; the projection then
			// offers no merge choice (component-http-api).
			log.Printf("httpapi: task detail merge readiness: request_id=%q task=%s error=%q", middleware.GetReqID(r.Context()), id, redactedErrorLogText(readinessErr))
		default:
			mergeReadiness = &readiness
			// A hook call that finished after the task read may have written
			// task state (component-submission-merge), so read it once more.
			if fresh {
				if refreshed, refreshErr := s.Store.GetTask(r.Context(), id); refreshErr == nil {
					task = refreshed
				}
			}
		}
	}
	jobs, err := s.Store.ListJobs(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	contextRequirements, contextDesigns := store.ActiveTaskContextReferences(events)
	contextProposals, err := s.Store.ListTaskContextProposals(r.Context(), id, core.TaskContextProposalProposed)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	interventions, err := s.Store.ListInterventions(r.Context(), id)
	if err == nil {
		err = s.annotateInterventionActorOwners(r.Context(), interventions)
	}
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	spec, hasSpec, err := s.Store.GetLatestSpecVersion(r.Context(), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	var specPointer *core.SpecVersion
	if hasSpec {
		spec.MaterializedChildren = materializedChildrenForSpec(task.Children, spec.Version)
		specPointer = &spec
	}
	workOrders, err := s.Store.ListTaskWorkOrders(store.WithWorkspace(r.Context(), task.Workspace), id)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if workOrders == nil {
		workOrders = []core.WorkOrder{}
	}
	projectionOrders := nextActionOrders(workOrders)
	pendingPlanRevision := false
	if task.State == core.TaskAwaiting {
		_, pending, planErr := store.PlanRevisionGateState(task, events)
		if errors.Is(planErr, store.ErrPlanRevisionGateUnreadable) {
			pending, planErr = true, nil
		}
		if planErr != nil {
			writeStoreFailure(w, r, planErr)
			return
		}
		pendingPlanRevision = pending
	}
	for index := range workOrders {
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
	// One metadata read serves both the task context and the checkpoint
	// citations, keyed by tier so a shared ID keeps both rows.
	metadata := store.NewDocumentMetadataSet()
	if documentIDs := taskDetailDocumentIDs(contextRequirements, contextDesigns, workOrders); len(documentIDs) > 0 {
		if metadata, err = s.Store.ListDocumentMetadata(r.Context(), documentIDs); err != nil {
			writeStoreFailure(w, r, err)
			return
		}
	}
	task.Context = taskDetailContext(contextRequirements, contextDesigns, metadata)
	task.Context.Proposals = contextProposals
	// Attachments are the operator-supplied task_context artifacts uploaded at
	// intake; the task detail view previews them below the execution plan.
	// Conveyor-generated audit transcripts are deliberately excluded — they are
	// evidence, not attachments. One task-node read serves both lists.
	artifacts, err := s.Store.ListArtifactsForLineage(r.Context(), []core.LineageNode{{Type: core.LineageTask, ID: id}})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	attachments, verificationEvidence := taskDetailArtifacts(id, artifacts)
	stalled := store.StalledTask(workOrders)
	if core.TaskTerminal(task.State) {
		stalled = nil
	}
	proposals, err := s.Store.ListPendingProposals(r.Context())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	workOrderViews, err := checkpointViewsFromMetadata(workOrders, proposals, metadata)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts, err := s.detailNextActionOptions(r.Context())
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	action, err := store.ReadNextActionForTask(r.Context(), s.Store, task, projectionOrders, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	compact := nextaction.CompactFrom(action)
	item := reviewItem{
		Task: task, Jobs: jobs, Events: events, Interventions: interventions,
		CheckoutCommand: checkoutCommand, CheckoutAvailable: checkoutAvailable, CheckoutGuidance: checkoutGuidance,
		NeedsAttention:            action.Actor == nextaction.ActorOperator,
		NextAction:                &compact,
		AtMergeGate:               store.AtMergeGate(task, events),
		ForgeFailure:              store.LatestForgeFailure(events),
		Spec:                      specPointer,
		WorkOrders:                workOrderViews,
		ReviewDiagnostics:         store.ReviewVerdictDiagnostics(workOrders, events, time.Now().UTC()),
		ReviewRecovery:            store.ReviewRecoveryNeeded(workOrders, events),
		InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(workOrders, events), events),
		Stalled:                   stalled,
		MergeReadiness:            mergeReadiness,
		Attachments:               attachments,
		VerificationEvidence:      verificationEvidence,
	}
	if !bounded {
		if err := s.annotateEventActorOwners(r.Context(), item.Events); err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, taskDetailProjection{item})
		return
	}
	// Every derivation above ran on the complete ledger; the bounded view
	// changes only how events are presented and adds the summaries.
	pageEvents, eventPage, err := s.boundedTaskEventSection(r, id, events)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	summaries := deriveTaskDetailSummaries(events, jobs, workOrders)
	if err := s.enrichReviewSeatSummaries(r.Context(), task, workOrders, events, &summaries); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if err := s.annotateSummaryActorOwners(r.Context(), &summaries); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	item.Events = nil
	w.Header().Set("X-Conveyor-Detail-View", "bounded")
	writeJSON(w, http.StatusOK, boundedTaskDetailProjection{reviewItem: item, events: pageEvents, eventPage: eventPage, summaries: summaries})
}

func decorateWorkOrderAgentActivity(orders []core.WorkOrder, events []core.Event) {
	byJob := make(map[string][]int, len(orders))
	for i := range orders {
		byJob[orders[i].JobID] = append(byJob[orders[i].JobID], i)
	}
	for _, event := range events {
		label := agentActivityLabel(event)
		if label == "" {
			continue
		}
		for _, index := range byJob[event.JobID] {
			if orders[index].LastAgentActivityAt.After(event.At) {
				continue
			}
			orders[index].LastAgentActivityAt = event.At
			orders[index].LastAgentActivityLabel = label
		}
	}
}

func agentActivityLabel(event core.Event) string {
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	switch event.Kind {
	case "work_order.claimed":
		return "Work order claimed"
	case "work_order.lease_renewed":
		return "Claim lease renewed"
	case "work_order.attempt_checkpointed":
		return "Attempt worktree checkpointed"
	case "work_order.progress_reported":
		message, _ := payload["message"].(string)
		message = strings.TrimSpace(message)
		if len(message) > 120 {
			limit := 120
			for limit > 0 && !utf8.RuneStart(message[limit]) {
				limit--
			}
			message = message[:limit] + "…"
		}
		if message != "" {
			return "Progress: " + message
		}
		return "Progress reported"
	case "work_order.usage_reported":
		if payload["rate_limit"] != nil {
			return "Usage and rate-limit telemetry reported"
		}
		return "Usage telemetry reported"
	case "transcript.self_reported":
		return "Transcript uploaded"
	case "pull_request.opened":
		return "Implementation submitted for review"
	case "review.completed", "review.accepted":
		return "Review verdict submitted"
	default:
		return ""
	}
}

// taskAttachments lists the operator-supplied attachments linked to a task,
// each with a download URL the dashboard can preview. Only task_context
// artifacts are surfaced; generated audit transcripts stay out of the view.
func (s *Server) checkoutState(taskID string) (string, bool, string) {
	command, available, guidance := checkoutStateFromHistory(taskID, nil)
	return command, available, guidance
}

func checkoutStateFromHistory(taskID string, _ []core.Event) (string, bool, string) {
	// An explicit run enters the governed work-order lifecycle and resolves its
	// own dedicated checkout when implementation begins (component-http-api).
	return "conveyor run " + taskID, true, "Runs the task through its governed work-order lifecycle."
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
