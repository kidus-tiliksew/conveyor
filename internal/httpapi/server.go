// Package httpapi is the Phase 2 control-plane REST + SSE surface and embedded
// activity/review SPA (design-http-api; design-web-dashboard). Mutations are authenticated and
// recorded with actor identity in the append-only event stream.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type Server struct {
	Store       store.Store
	Credentials CredentialVerifier
	// Release is the build-injected binary identity reported by /v1/version.
	Release                  string
	planningBundleMu         sync.Mutex
	planningBundleDispatched map[string]struct{}
	// Repos is the set of valid repo names; nil skips validation.
	Repos []string
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
	OnMerge          func(context.Context, core.Task) error
	OnMergeReadiness func(context.Context, core.Task) (dispatch.MergeReadiness, error)
	OnConflictFix    func(context.Context, core.Task) (core.WorkOrder, error)
	// Workspace stamps created tasks.
	Workspace string
	// BearerToken authenticates mutating requests (design-http-api). An empty
	// token denies all mutations rather than silently disabling auth.
	BearerToken string
	// WorkspaceInfo is the static fallback for the unauthenticated display
	// snapshot; production resolves it dynamically through ConfigProvider.
	WorkspaceInfo *WorkspaceInfo
	// ConfigProvider resolves current database-backed workspace scope while
	// preserving file-backed deployment fields.
	ConfigProvider        func(context.Context) (*config.Config, error)
	ConfigStore           WorkspaceConfigStore
	Workspaces            store.WorkspaceControlStore
	Memberships           store.MembershipStore
	IdentityProvisioner   store.IdentityProvisioner
	CallerIdentities      store.CallerIdentityStore
	PersonalTokens        store.PersonalAccessTokenStore
	InvitationSessions    store.InvitationSessionStore
	InvitationDelivery    config.InvitationDelivery
	EnsureWorkspaceQueues func(string) error
	Deployment            *config.Config
	WorkOrders            *workorder.Service
	Workers               *workerservice.Service
	Monitor               *monitor.Service
	Planning              *planning.Service
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
	if identities, ok := s.(store.IdentityProvisioner); ok {
		server.IdentityProvisioner = identities
	}
	if identities, ok := s.(store.CallerIdentityStore); ok {
		server.CallerIdentities = identities
	}
	if tokens, ok := s.(store.PersonalAccessTokenStore); ok {
		server.PersonalTokens = tokens
	}
	if sessions, ok := s.(store.InvitationSessionStore); ok {
		server.InvitationSessions = sessions
	}
	return server
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"version": s.Release})
		})
		r.Post("/worker/enroll", s.enrollWorker)
		r.Post("/sign-in/redeem", s.redeemSignInLink)
		r.With(s.requireSelfServiceCredential).Post("/sign-out", s.signOutDashboardSession)
		r.With(s.requireWorkerAuth).Post("/worker/heartbeat", s.heartbeatWorker)
		r.With(s.requireWorkerAuth).Get("/worker/config", s.getWorkerConfig)
		r.With(s.requireWorkerAuth).Get("/worker/work-orders", s.listWorkerOrders)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/claim", s.claimWorkerOrder)
		r.With(s.requireWorkerAuth).Get("/worker/work-orders/{id}/reconcile", s.reconcileWorkerOrder)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/renew", s.renewWorkerOrder)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/attempt-checkpoint", s.checkpointWorkerOrderAttempt)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/release", s.releaseWorkerOrder)
		r.With(s.requireWorkspaceAuth).Get("/workspaces", s.listWorkspaces)
		r.With(s.requireSelfServiceCredential, s.resolveOptionalWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/me", s.getCallerIdentity)
		r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/users", s.provisionIdentityUser)
		// Self-service credential routes carry no subject in the path: the owner
		// is the presented credential, so no capability applies and no request
		// can name another user's tokens (REQ-2/AC-2.1).
		r.With(s.requireSelfServiceCredential).Get("/tokens", s.listOwnPersonalAccessTokens)
		r.With(s.requireSelfServiceCredential).Post("/tokens", s.issueOwnPersonalAccessToken)
		r.With(s.requireSelfServiceCredential).Delete("/tokens/{token_id}", s.revokeOwnPersonalAccessToken)
		r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/workspaces", s.createWorkspace)
		r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Get("/harness-templates", s.getHarnessTemplates)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}", s.getWorkspaceRecord)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/config", s.getWorkspaceConfig)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageWorkspace)).Put("/workspaces/{workspace_id}/config", s.putWorkspaceConfig)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace)).Get("/workspaces/{workspace_id}/members", s.listWorkspaceMembers)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Post("/workspaces/{workspace_id}/members", s.grantWorkspaceMembership)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Get("/workspaces/{workspace_id}/invitations", s.listWorkspaceInvitations)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Delete("/workspaces/{workspace_id}/invitations/{email}", s.revokeWorkspaceInvitation)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Post("/workspaces/{workspace_id}/invitations/{email}/resend", s.resendWorkspaceInvitation)
		r.With(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityManageMembership)).Delete("/workspaces/{workspace_id}/members/{user_id}", s.revokeWorkspaceMembership)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorkspaceAuth, s.resolveWorkspaceContext, s.requireWorkspaceCapability(core.CapabilityViewWorkspace))
			r.Get("/activity", s.listActivity)
			r.Get("/pending-proposals", s.listPendingProposals)
			r.Get("/tasks", s.listTasks)
			r.Get("/task-operations", s.listTaskOperations)
			r.Get("/tasks/{id}", s.getTask)
			r.Get("/tasks/{id}/jobs", s.listJobs)
			r.Get("/tasks/{id}/events", s.listEvents)
			r.Get("/tasks/{id}/events/stream", s.streamEvents)
			r.Get("/tasks/{id}/activity", s.getTaskActivity)
			r.Get("/lineage/{type}/{id}", s.getLineage)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/lineage/rebuild", s.rebuildLineage)
			r.Get("/tasks/{id}/interventions", s.listInterventions)
			r.Get("/tasks/{id}/spec", s.getLatestSpec)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Get("/tasks/{id}/run-order", s.getTaskRunOrder)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/run-orders/{order_id}/claim", s.claimTaskRunOrder)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/run-orders/{order_id}/renew", s.renewTaskRunOrder)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Get("/tasks/{id}/run-orders/{order_id}/reconcile", s.reconcileTaskRunOrder)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/run-orders/{order_id}/attempt-checkpoint", s.checkpointTaskRunOrderAttempt)
			r.With(s.requireTaskRunAuth, s.requireWorkspaceCapability(core.CapabilityClaimWork)).Post("/tasks/{id}/run-orders/{order_id}/release", s.releaseTaskRunOrder)
			// Request-changes is the human merge-gate action, not part of the
			// bearer-only run-order automation plane. Dashboard sessions retain
			// this route through the outer CSRF proof and capability boundary.
			r.With(s.requireTaskRequestChangesAuth, s.requireWorkspaceCapability(core.CapabilityRequestChanges)).Post("/tasks/{id}/request-changes", s.requestTaskChanges)
			r.Get("/reviews", s.listReviews)
			r.Get("/workspace", s.getWorkspace)
			r.Get("/work-orders", s.listWorkOrders)
			r.Get("/monitor", s.getMonitorStatus)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/work-orders/{id}/recover", s.recoverWorkOrder)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/work-orders/{id}/preempt", s.preemptWorkOrder)
			r.Get("/blueprints", s.listBlueprints)
			r.Get("/requirements", s.listRequirements)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/requirements", s.createRequirement)
			r.Get("/requirements/{id}", s.getRequirement)
			r.Get("/requirements/{id}/versions", s.listRequirementVersions)
			r.Get("/requirements/{id}/checkpoint-context-candidates", s.listCheckpointContextCandidates)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/requirements/{id}/versions", s.proposeRequirementVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/requirements/{id}/versions/{version}/confirm", s.confirmRequirementVersion)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/requirements/{id}/staleness/{signal}/acknowledge", s.acknowledgeRequirementStaleness)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/requirements/{id}/staleness/{signal}/follow-up", s.createRequirementStalenessFollowUp)
			r.Get("/reference-documents", s.listReferenceDocuments)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/reference-documents", s.createReferenceDocument)
			r.Get("/reference-documents/{id}/versions", s.listReferenceDocumentVersions)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/reference-documents/{id}/versions", s.supersedeReferenceDocument)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Delete("/reference-documents/{id}", s.deleteReferenceDocument)
			r.Get("/system-designs", s.listSystemDesigns)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/system-designs", s.createSystemDesign)
			r.Get("/system-designs/{id}", s.getSystemDesign)
			r.Get("/system-designs/{id}/versions", s.listSystemDesignVersions)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/system-designs/{id}/versions", s.proposeSystemDesignVersion)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/system-designs/{id}/versions/{version}/confirm", s.confirmSystemDesignVersion)
			r.Get("/decisions", s.listDecisions)
			r.With(s.requireMutationCapability(core.CapabilityProposeDocuments)).Post("/decisions", s.proposeDecision)
			r.Get("/decisions/{id}", s.getDecision)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/decisions/{id}/confirm", s.confirmDecision)
			r.With(s.requireMutationCapability(core.CapabilityConfirmDocuments)).Post("/decisions/{id}/dismiss", s.dismissDecision)
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
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks", s.createTask)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/monitor/observations", s.observeMonitorSignal)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/monitor/drift/{id}/resolve", s.resolveMonitorDrift)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/redispatch", s.redispatchTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Put("/tasks/{id}/hold", s.setTaskHold)
			r.With(s.requireMutationCapability(core.CapabilitySetAssignee)).Put("/tasks/{id}/assignee", s.setTaskAssignee)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/context", s.updateTaskContext)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/setup", s.changeTaskSetup)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Delete("/tasks/{id}/dependencies/{dependency_id}", s.removeTaskDependency)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/review-round/retry", s.retryReviewRound)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/review-round/recover", s.recoverInterruptedReviewRound)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/review", s.reviewTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/close", s.closeTask)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/tasks/{id}/merge", s.mergeTask)
			r.With(s.requireMutationCapability(core.CapabilityRecoverWork)).Post("/tasks/{id}/merge-conflict-fix", s.fixMergeConflict)
			r.Get("/artifacts", s.listArtifacts)
			r.With(s.requireMutationCapability(core.CapabilityOperateGates)).Post("/artifacts", s.uploadArtifact)
			r.Get("/artifacts/{id}", s.downloadArtifact)
			r.Get("/workers", s.listWorkers)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Post("/workers/pairings", s.issueWorkerPairing)
			r.With(s.requireMutationCapability(core.CapabilityManageWorkspace)).Delete("/workers/{id}", s.revokeWorker)
		})
	})
	// All methods route to the handler so non-POST gets a spec-correct 405
	// (streamable-HTTP clients probe GET for an SSE stream); registering
	// only Post would let GET fall through to the SPA catch-all as 200 HTML.
	r.With(s.requireMCPAuth).HandleFunc("/mcp", s.handleMCP)
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
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if t.State == core.TaskRunning {
		http.Error(w, "task is already active", http.StatusConflict)
		return
	}
	if t.State == core.TaskQueued {
		if err := s.Store.EnsureTaskEnqueued(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			log.Printf("ensure task enqueued: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	} else {
		nextStage := t.NextStage
		if t.State == core.TaskParked && nextStage == "" {
			nextStage = t.RecoveryStage
		}
		if nextStage == "" {
			http.Error(w, "task has no decided next stage; record an explicit review redirect instead", http.StatusConflict)
			return
		}
		command := core.TaskRecover
		switch t.State {
		case core.TaskAwaiting:
			command = core.TaskInterventionRedirect
		case core.TaskApproved:
			command = core.TaskRefreshReview
		case core.TaskClaiming:
			command = core.TaskIntakeFinalize
		}
		var transitionErr error
		if t.State == core.TaskParked {
			// A triage park records the stage in RecoveryStage and clears
			// NextStage. Recovery restores that stage as the next dispatch
			// target (design-task-lifecycle).
			_, transitionErr = taskops.New(s.Store).Perform(r.Context(), id, taskops.Command{Kind: command, NextStage: nextStage, ProjectStages: true})
		} else {
			_, transitionErr = taskops.New(s.Store).Perform(r.Context(), id, taskops.Command{Kind: command})
		}
		if transitionErr != nil {
			status := http.StatusInternalServerError
			var invalidTransition *core.ErrInvalidTransition
			if errors.As(transitionErr, &invalidTransition) {
				status = http.StatusConflict
			}
			http.Error(w, transitionErr.Error(), status)
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

// setTaskHold toggles the §21.31 per-task reservation: while held, workers
// never claim the task's work orders. Hold is deliberately mutable after
// intake (§21.31 change 5) and every toggle is audited by the store.
func (s *Server) setTaskHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Hold *bool `json:"hold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Hold == nil {
		http.Error(w, "hold is required", http.StatusBadRequest)
		return
	}
	task, err := s.Store.SetTaskHold(r.Context(), id, *req.Hold)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) setTaskAssignee(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AssigneeUserID string `json:"assignee_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	task, err := taskops.New(s.Store).SetAssignee(r.Context(), chi.URLParam(r, "id"), request.AssigneeUserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
				credential, err = s.authenticateHumanCredential(r)
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
	// Unit-test and explicit memory stores have no durable workspace registry;
	// production multi-workspace reads follow the authenticated operator policy.
	if s.Workspaces == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, err := s.authenticateHumanCredential(r)
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

// requireSelfServiceCredential authenticates the human credential that owns a
// self-service request. It never resolves a workspace and never consults a
// capability bundle: the caller's authority over the resource is the fact that
// the resource is theirs. Agent credentials are refused so an execution session
// cannot enumerate or revoke its owner's tokens (REQ-2/AC-2.2).
func (s *Server) requireSelfServiceCredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, err := s.authenticateHumanCredential(r)
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
				next.ServeHTTP(w, r)
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
		if verifyErr == nil {
			actor := store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser}
			if credential.Kind == core.CredentialAgent {
				actor = store.Actor{ID: store.AgentActorID(credential.ID), Role: core.ActorAgent}
			}
			ctx := store.WithCredential(r.Context(), credential)
			ctx = store.WithActor(ctx, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if !errors.Is(verifyErr, core.ErrInvalidCredential) {
			writeCredentialVerificationError(w, verifyErr)
			return
		}
		if s.Workers != nil {
			ctx, worker, err := s.Workers.Authenticate(r.Context(), provided, "")
			if err == nil {
				ctx = context.WithValue(ctx, workerContextKey{}, worker)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
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

func (s *Server) authenticateHumanCredential(r *http.Request) (core.AuthenticatedCredential, error) {
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
	return s.InvitationSessions.VerifyDashboardSession(r.Context(), cookie.Value)
}

func (s *Server) requireSessionMutationProof(w http.ResponseWriter, r *http.Request, credential core.AuthenticatedCredential) bool {
	if credential.Method != core.CredentialMethodSession || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if r.Header.Get("X-Conveyor-CSRF") != "1" {
		http.Error(w, "CSRF proof required", http.StatusForbidden)
		return false
	}
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

func (s *Server) removeTaskDependency(w http.ResponseWriter, r *http.Request) {
	var request removeTaskDependencyRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.Store.RemoveTaskDependency(r.Context(), store.DependencyRemovalRequest{
		TaskID: chi.URLParam(r, "id"), DependsOnTaskID: chi.URLParam(r, "dependency_id"),
		Reason: request.Reason, RequestID: request.RequestID,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) closeTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if core.TaskTerminal(task.State) {
		http.Error(w, store.ErrTaskTerminal.Error(), http.StatusConflict)
		return
	}
	var request closeTaskRequest
	if err = json.NewDecoder(r.Body).Decode(&request); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = strings.TrimSpace(request.ReasonCode)
	}
	if reason == "" || len(reason) > 64 {
		http.Error(w, "reason is required and must be at most 64 characters", http.StatusBadRequest)
		return
	}
	latest, hasJob, err := s.Store.GetLatestJob(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrTaskTerminal) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, updated)
}

func (s *Server) reviewTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !reviewable(task.State) {
		http.Error(w, "task is not at a human gate", http.StatusConflict)
		return
	}
	_, planRevisionGate, err := dispatch.PendingPlanRevisionGate(r.Context(), s.Store, id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var request reviewRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !request.Action.Valid() || request.Action == core.InterventionCancel {
		http.Error(w, "invalid review action", http.StatusBadRequest)
		return
	}
	if task.State == core.TaskApproved && request.Action == core.InterventionApprove {
		http.Error(w, "approved tasks must use the merge operation", http.StatusConflict)
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
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
			log.Printf("create plan revision intervention: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		interventionRecorded = true
	}
	if s.OnIntervention != nil {
		if err := s.OnIntervention(r.Context(), task, latestJob, intervention); err != nil {
			status := http.StatusInternalServerError
			var invalidTransition *core.ErrInvalidTransition
			if errors.As(err, &invalidTransition) || errors.Is(err, dispatch.ErrReviewedHeadUnavailable) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	}
	if !interventionRecorded {
		if err := s.Store.CreateIntervention(r.Context(), intervention); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			log.Printf("create intervention: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	if s.OnIntervention == nil && request.Action == core.InterventionRedirect && s.OnCreate != nil {
		s.OnCreate(r.Context(), id)
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusNotFound)
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
	if err := s.OnMerge(r.Context(), task); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if updated.State != core.TaskMerged {
		http.Error(w, "merge was not confirmed by the forge", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) fixMergeConflict(w http.ResponseWriter, r *http.Request) {
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if s.OnConflictFix == nil {
		http.Error(w, "merge conflict dispatch is not configured", http.StatusNotImplemented)
		return
	}
	order, err := s.OnConflictFix(r.Context(), task)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, order)
}

func reviewable(state core.TaskState) bool {
	return state == core.TaskAwaiting || state == core.TaskApproved
}

type createTaskReq struct {
	Body            string               `json:"body"`
	Repo            string               `json:"repo"`
	BaseBranch      string               `json:"base_branch"`
	Source          string               `json:"source"`
	Level           core.EscalationLevel `json:"level"`
	Hold            bool                 `json:"hold"`
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
			return fmt.Errorf("mode is retired; use hold when reserving a task")
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.createTaskRecord(r.Context(), req, "", "api")
	if err != nil {
		writeTaskCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result.Task)
}

func (s *Server) createTaskWithAttachments(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxArtifactBytes); err != nil {
		http.Error(w, "invalid attachment task form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	var req createTaskReq
	if err := json.Unmarshal([]byte(r.FormValue("task")), &req); err != nil {
		http.Error(w, "invalid task metadata: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.createTaskRecordWithState(r.Context(), req, r.FormValue("idempotency_key"), "api", core.TaskClaiming)
	if err != nil {
		writeTaskCreateError(w, err)
		return
	}
	if !result.Created && result.Task.State != core.TaskClaiming {
		writeJSON(w, http.StatusOK, result.Task)
		return
	}
	for _, header := range r.MultipartForm.File["attachments"] {
		if _, err = s.storeTaskAttachment(r, result.Task, header); err != nil {
			http.Error(w, fmt.Sprintf("task %s remains unqueued because attachment %s failed: %v", result.Task.ID, safeFilename(header), err), http.StatusUnprocessableEntity)
			return
		}
	}
	if _, err = taskops.New(s.Store).Perform(r.Context(), result.Task.ID, taskops.Command{Kind: core.TaskIntakeFinalize}); err != nil {
		http.Error(w, fmt.Sprintf("task %s remains unqueued because finalization failed: %v", result.Task.ID, err), http.StatusInternalServerError)
		return
	}
	result.Task.State = core.TaskQueued
	if s.OnCreate != nil {
		s.OnCreate(r.Context(), result.Task.ID)
	}
	writeJSON(w, http.StatusCreated, result.Task)
}

func (s *Server) storeTaskAttachment(r *http.Request, task core.Task, header *multipart.FileHeader) (core.Artifact, error) {
	file, err := header.Open()
	if err != nil {
		return core.Artifact{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return core.Artifact{}, err
	}
	if len(content) > maxArtifactBytes {
		return core.Artifact{}, fmt.Errorf("artifact exceeds 25 MiB")
	}
	contentType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(content)
	}
	return s.Store.CreateArtifact(r.Context(), core.Artifact{Workspace: task.Workspace, Name: safeFilename(header), ContentType: contentType, SizeBytes: int64(len(content)), TaskID: task.ID, CreatedAt: time.Now().UTC()}, content)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	t.Context, err = store.TaskContextForTask(r.Context(), s.Store, t.ID)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListJobs(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.ListEvents(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
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
	lastID := int64(0)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		events, err := s.Store.ListEventsAfter(r.Context(), chi.URLParam(r, "id"), lastID)
		if err != nil {
			_, _ = w.Write([]byte("event: error\ndata: {}\n\n"))
			flusher.Flush()
			return
		}
		for _, event := range events {
			if event.ID <= lastID {
				continue
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			_, _ = w.Write([]byte("event: activity\ndata: "))
			_, _ = w.Write(data)
			_, _ = w.Write([]byte("\n\n"))
			lastID = event.ID
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) listInterventions(w http.ResponseWriter, r *http.Request) {
	interventions, err := s.Store.ListInterventions(r.Context(), chi.URLParam(r, "id"))
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
	AtMergeGate               bool                                  `json:"at_merge_gate"`
	PendingAuthority          bool                                  `json:"pending_authority"`
	ForgeFailure              *store.ForgeFailure                   `json:"forge_failure,omitempty"`
	Spec                      *core.SpecVersion                     `json:"spec,omitempty"`
	WorkOrders                []core.WorkOrder                      `json:"work_orders"`
	ReviewDiagnostics         []store.ReviewVerdictDiagnostic       `json:"review_diagnostics,omitempty"`
	ReviewRecovery            *store.ReviewRecoveryState            `json:"review_recovery,omitempty"`
	InterruptedReviewRecovery *store.InterruptedReviewRecoveryState `json:"interrupted_review_recovery,omitempty"`
	Stalled                   *store.StalledState                   `json:"stalled,omitempty"`
	WorkerStatus              *workerservice.TaskWorkerStatus       `json:"worker_status,omitempty"`
	MergeReadiness            *dispatch.MergeReadiness              `json:"merge_readiness,omitempty"`
	Attachments               []core.Artifact                       `json:"attachments"`
	VerificationEvidence      []core.Artifact                       `json:"verification_evidence"`
}

type activityItem struct {
	Task                      core.Task                             `json:"task"`
	LatestStage               core.Stage                            `json:"latest_stage,omitempty"`
	LastEventAt               time.Time                             `json:"last_event_at"`
	NeedsAttention            bool                                  `json:"needs_attention"`
	PendingAuthority          bool                                  `json:"pending_authority"`
	ForgeFailure              *store.ForgeFailure                   `json:"forge_failure,omitempty"`
	ReviewDiagnostics         []store.ReviewVerdictDiagnostic       `json:"review_diagnostics,omitempty"`
	ReviewRecovery            *store.ReviewRecoveryState            `json:"review_recovery,omitempty"`
	InterruptedReviewRecovery *store.InterruptedReviewRecoveryState `json:"interrupted_review_recovery,omitempty"`
	Stalled                   *store.StalledState                   `json:"stalled,omitempty"`
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
	filter, err := parseTaskFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.listActivityFiltered(w, r, filter, false)
}

func (s *Server) listActivityFiltered(w http.ResponseWriter, r *http.Request, filter store.TaskFilter, reviewsOnly bool) {
	tasks, err := s.Store.ListTasksFiltered(r.Context(), filter)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markers, err := s.Store.ListActivityMarkers(r.Context())
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	proposals, err := s.Store.ListPendingProposals(r.Context())
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	pendingAuthority, err := s.pendingAuthorityTasks(r.Context(), proposals)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	items := make([]activityItem, 0, len(tasks))
	for _, task := range tasks {
		// Blueprint anchors are intent artifacts, not claimable work, so they
		// leave the stage-grouped board and its counts for the Blueprints
		// surface (design-web-dashboard). The review inbox projection keeps them: an
		// anchor at its spec gate has not materialized children yet, so it is
		// not classified here and its approval card is untouched.
		if !reviewsOnly && core.BlueprintAnchor(task) {
			continue
		}
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		if reviewsOnly && !reviewable(task.State) && marker.ForgeFailure == nil && marker.ReviewRecovery == nil && marker.InterruptedReviewRecovery == nil && marker.Stalled == nil && !marker.UserChangesRequested && !pendingAuthority[task.ID] {
			continue
		}
		// Project the existing presentation-only authority signal without
		// changing any lifecycle gate (REQ-2 AC-2.2; REQ-3; design-web-dashboard).
		items = append(items, activityItem{
			Task: task, LatestStage: marker.LatestStage, LastEventAt: marker.LastEventAt,
			NeedsAttention:            needsAttention(task, marker, pendingAuthority[task.ID]),
			PendingAuthority:          pendingAuthority[task.ID],
			ForgeFailure:              marker.ForgeFailure,
			ReviewDiagnostics:         marker.ReviewDiagnostics,
			ReviewRecovery:            marker.ReviewRecovery,
			InterruptedReviewRecovery: marker.InterruptedReviewRecovery,
			Stalled:                   marker.Stalled,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// needsAttention is the one derivation the stage-grouped board and the
// list-first Tasks view share, so the two surfaces cannot disagree about which
// work is waiting on a human (design-web-dashboard).
func needsAttention(task core.Task, marker store.ActivityMarker, pendingAuthority bool) bool {
	return store.TaskNeedsAttention(task, marker, pendingAuthority)
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

// taskOperationsItem is the list-first Tasks view's row (design-web-dashboard).
// Every field projects durable task, relationship, context, or plan authority;
// the view stores nothing and re-derives nothing of its own. No priority or
// declared-phase field appears here; assignee is durable task authority.
type taskOperationsItem struct {
	Task        core.Task  `json:"task"`
	LatestStage core.Stage `json:"latest_stage,omitempty"`
	LastEventAt time.Time  `json:"last_event_at"`
	// Stalled says why a row cannot move on its own, so staleness is legible
	// from the task-level surface. The authority is the
	// same derived §21.34 state the board and task detail read; only the fields
	// a list row states travel with it.
	Stalled              *taskStalledSummary `json:"stalled,omitempty"`
	NeedsAttention       bool                `json:"needs_attention"`
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

// listTaskOperations serves the list-first Tasks view (design-web-dashboard).
// Dependencies, blocking edges, and children come from a bounded store page;
// attached context and plan status fold only the event kinds and latest plan
// records the projection batches for those returned tasks. The historical
// blueprint read path is not a scale precedent for this daily surface.
func (s *Server) listTaskOperations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query, paginated, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, err := s.Store.ListTaskOperations(ctx, query)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if paginated {
		w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
		w.Header().Set("X-Conveyor-Limit", strconv.Itoa(query.Limit))
		w.Header().Set("X-Conveyor-Offset", strconv.Itoa(query.Offset))
	}
	taskIDs := make([]string, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	markers, err := s.Store.ListActivityMarkersForTasks(ctx, taskIDs)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	proposals, err := s.Store.ListPendingProposals(ctx)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	pendingAuthority, err := s.pendingAuthorityTasks(ctx, proposals)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Blocked stays a derived predicate owned by the dependency substrate
	// (design-task-lifecycle); the view reads it, and reads the unsatisfiable edges the
	// task record itself does not carry.
	blockers, err := s.Store.ListDependencyBlockers(ctx, taskIDs)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Attached documents repeat across rows; memoizing only the document reads
	// keeps the authoritative fold in store.TaskContextFromEvents.
	documents := &taskContextDocumentCache{Store: s.Store, requirements: map[string]core.Requirement{}, designs: map[string]core.SystemDesign{}}
	items := make([]taskOperationsItem, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		events := page.Events[task.ID]
		task.Context, err = store.TaskContextFromEvents(ctx, documents, events)
		if err != nil {
			log.Printf("handle API request: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		latest, hasPlan := page.Plans[task.ID]
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		items = append(items, taskOperationsItem{
			Task:                 task,
			LatestStage:          marker.LatestStage,
			LastEventAt:          marker.LastEventAt,
			Stalled:              taskStalledSummaryFor(marker.Stalled),
			NeedsAttention:       needsAttention(task, marker, pendingAuthority[task.ID]),
			UnsatisfiableTaskIDs: blockers[task.ID].UnsatisfiableTaskIDs,
			ChildRollup:          taskChildRollupFor(task.Children),
			Plan:                 taskPlanStatusFor(latest, hasPlan, events),
		})
	}
	writeJSON(w, http.StatusOK, items)
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
	// Updated bounds previously meant last activity. Rejecting the retired
	// spelling keeps stale API callers from silently receiving Created semantics
	// or an unfiltered result; saved browser state is migrated by the UI.
	if values.Has("updated_from") || values.Has("updated_to") {
		return filter, fmt.Errorf("updated_from and updated_to are retired; use created_from and created_to")
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
	var err error
	if filter.CreatedFrom, err = parseTaskFilterInstant(values.Get("created_from")); err != nil {
		return filter, err
	}
	if filter.CreatedTo, err = parseTaskFilterInstant(values.Get("created_to")); err != nil {
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
func parseTaskFilterInstant(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("updated bounds must be RFC 3339 instants: %w", err)
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
	limitValue, paginated := values["limit"]
	if !paginated {
		if values.Get("offset") != "" {
			return query, false, fmt.Errorf("offset requires limit")
		}
		return query, false, nil
	}
	if len(limitValue) != 1 {
		return query, false, fmt.Errorf("limit must be supplied once")
	}
	limit, err := strconv.Atoi(limitValue[0])
	if err != nil || limit < 1 || limit > store.MaxTaskOperationsLimit {
		return query, false, fmt.Errorf("limit must be between 1 and %d", store.MaxTaskOperationsLimit)
	}
	offset := 0
	if value := values.Get("offset"); value != "" {
		// Bound the offset here, before it reaches a store: past
		// store.MaxTaskOperationsOffset the two stores disagree, because
		// Postgres narrows the bind parameter to int32.
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > store.MaxTaskOperationsOffset {
			return query, false, fmt.Errorf("offset must be between 0 and %d", store.MaxTaskOperationsOffset)
		}
	}
	query.Limit, query.Offset = limit, offset
	return query, true, nil
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
// (design-task-lifecycle).
func taskPlanStatusFor(latest core.SpecVersion, hasPlan bool, events []core.Event) taskPlanStatus {
	if !hasPlan {
		return taskPlanStatus{State: taskPlanNone}
	}
	status := taskPlanStatus{State: taskPlanApproved, Version: latest.Version, Legacy: latest.LegacyGate}
	if latest.Approved {
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
	id := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	jobs, err := s.Store.ListJobs(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	task.Context, err = store.TaskContextFromEvents(r.Context(), s.Store, events)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	interventions, err := s.Store.ListInterventions(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	spec, hasSpec, err := s.Store.GetLatestSpecVersion(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var specPointer *core.SpecVersion
	if hasSpec {
		spec.MaterializedChildren = materializedChildrenForSpec(task.Children, spec.Version)
		specPointer = &spec
	}
	workOrders, err := s.Store.ListTaskWorkOrders(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if workOrders == nil {
		workOrders = []core.WorkOrder{}
	}
	for index := range workOrders {
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
	// Attachments are the operator-supplied task_context artifacts uploaded at
	// intake; the task detail view previews them below the execution plan.
	// Conveyor-generated audit transcripts are deliberately excluded — they are
	// evidence, not attachments.
	attachments, err := s.taskAttachments(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	verificationEvidence, err := s.taskVerificationEvidence(r.Context(), id)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var workerStatus *workerservice.TaskWorkerStatus
	var mergeReadiness *dispatch.MergeReadiness
	if task.State == core.TaskApproved && s.OnMergeReadiness != nil {
		readiness, readinessErr := s.OnMergeReadiness(r.Context(), task)
		if readinessErr != nil {
			// The merge action must never render without a successful gate-facing
			// readiness read (design-git-delivery).
			http.Error(w, fmt.Sprintf("resolve merge readiness: %v", readinessErr), http.StatusServiceUnavailable)
			return
		}
		mergeReadiness = &readiness
		if refreshed, refreshErr := s.Store.GetTask(r.Context(), id); refreshErr == nil {
			task = refreshed
		}
	}
	// Worker status is advisory serviceability (§21.31); held tasks are the
	// operator's to claim, so no worker availability is reported for them.
	if s.Workers != nil && s.ConfigProvider != nil && !task.Hold && !core.TaskTerminal(task.State) {
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
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	pendingAuthority := pendingAuthorityForTask(id, workOrders, proposals)
	writeJSON(w, http.StatusOK, reviewItem{
		Task: task, Jobs: jobs, Events: events, Interventions: interventions,
		CheckoutCommand: checkoutCommand, CheckoutAvailable: checkoutAvailable, CheckoutGuidance: checkoutGuidance,
		NeedsAttention:            task.State == core.TaskAwaiting || task.State == core.TaskParked || store.LatestForgeFailure(events) != nil || store.ReviewRecoveryNeeded(workOrders, events) != nil || store.InterruptedReviewRecoveryNeeded(workOrders) != nil || stalled != nil || store.UserRequestChangesPending(events) || pendingAuthority,
		AtMergeGate:               store.AtMergeGate(task, events),
		PendingAuthority:          pendingAuthority,
		ForgeFailure:              store.LatestForgeFailure(events),
		Spec:                      specPointer,
		WorkOrders:                workOrders,
		ReviewDiagnostics:         store.ReviewVerdictDiagnostics(workOrders, events, time.Now().UTC()),
		ReviewRecovery:            store.ReviewRecoveryNeeded(workOrders, events),
		InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(store.CurrentReviewOrders(workOrders, events)),
		Stalled:                   stalled,
		WorkerStatus:              workerStatus,
		MergeReadiness:            mergeReadiness,
		Attachments:               attachments,
		VerificationEvidence:      verificationEvidence,
	})
}

func (s *Server) taskVerificationEvidence(ctx context.Context, taskID string) ([]core.Artifact, error) {
	all, err := s.Store.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	evidence := make([]core.Artifact, 0)
	for _, artifact := range all {
		if artifact.TaskID != taskID || !artifact.EligibleVerificationEvidence() {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		evidence = append(evidence, artifact)
	}
	return evidence, nil
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
func (s *Server) taskAttachments(ctx context.Context, taskID string) ([]core.Artifact, error) {
	all, err := s.Store.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	attachments := make([]core.Artifact, 0)
	for _, artifact := range all {
		if artifact.TaskID != taskID || artifact.Role != core.ArtifactRoleTaskContext {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		attachments = append(attachments, artifact)
	}
	return attachments, nil
}

func (s *Server) checkoutState(taskID string) (string, bool, string) {
	command, available, guidance := checkoutStateFromHistory(taskID, nil)
	return command, available, guidance
}

func checkoutStateFromHistory(taskID string, _ []core.Event) (string, bool, string) {
	// The checkout helper can safely create a missing assigned branch from the
	// freshly fetched base, so the dedicated-worktree command is available as
	// soon as the task exists (design-git-delivery).
	return "conveyor checkout " + taskID, true, "Creates or reuses the clean, task-dedicated worktree without switching the primary checkout."
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
