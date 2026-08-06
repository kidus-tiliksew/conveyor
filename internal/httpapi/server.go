// Package httpapi is the Phase 2 control-plane REST + SSE surface and embedded
// activity/review SPA (spec §13.3, §17.3). Mutations are authenticated and
// recorded with actor identity in the append-only event stream.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type Server struct {
	Store                    store.Store
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
	// OnIntervention validates fresh authoritative state and advances the gate
	// before the append-only decision is committed (spec §13.2).
	OnIntervention func(context.Context, core.Task, core.Job, core.Intervention) error
	// OnMerge performs and authoritatively confirms the final forge merge.
	OnMerge          func(context.Context, core.Task) error
	OnMergeReadiness func(context.Context, core.Task) (dispatch.MergeReadiness, error)
	OnConflictFix    func(context.Context, core.Task) (core.WorkOrder, error)
	// Workspace stamps created tasks.
	Workspace string
	// BearerToken authenticates mutating requests (spec §17.3). An empty
	// token denies all mutations rather than silently disabling auth.
	BearerToken string
	// WorkspaceInfo is the static fallback for the unauthenticated display
	// snapshot; production resolves it dynamically through ConfigProvider.
	WorkspaceInfo *WorkspaceInfo
	// ConfigProvider resolves current database-backed workspace scope while
	// preserving file-backed deployment fields (spec §21.3).
	ConfigProvider        func(context.Context) (*config.Config, error)
	ConfigStore           WorkspaceConfigStore
	Workspaces            store.WorkspaceControlStore
	EnsureWorkspaceQueues func(string) error
	Deployment            *config.Config
	WorkOrders            *workorder.Service
	Workers               *workerservice.Service
	Monitor               *monitor.Service
	Planning              *planning.Service
}

func NewServer(s store.Store) *Server { return &Server{Store: s} }

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/worker/enroll", s.enrollWorker)
		r.With(s.requireWorkerAuth).Post("/worker/heartbeat", s.heartbeatWorker)
		r.With(s.requireWorkerAuth).Get("/worker/config", s.getWorkerConfig)
		r.With(s.requireWorkerAuth).Get("/worker/work-orders", s.listWorkerOrders)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/claim", s.claimWorkerOrder)
		r.With(s.requireWorkerAuth).Get("/worker/work-orders/{id}/reconcile", s.reconcileWorkerOrder)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/renew", s.renewWorkerOrder)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/attempt-checkpoint", s.checkpointWorkerOrderAttempt)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/release", s.releaseWorkerOrder)
		r.With(s.requireMutationAuth).Get("/workspaces", s.listWorkspaces)
		r.With(s.requireMutationAuth).Post("/workspaces", s.createWorkspace)
		r.With(s.requireMutationAuth).Get("/harness-templates", s.getHarnessTemplates)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Get("/workspaces/{workspace_id}", s.getWorkspaceRecord)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Get("/workspaces/{workspace_id}/config", s.getWorkspaceConfig)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Put("/workspaces/{workspace_id}/config", s.putWorkspaceConfig)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorkspaceAuth, s.resolveWorkspaceContext)
			r.Get("/activity", s.listActivity)
			r.Get("/tasks", s.listTasks)
			r.Get("/task-operations", s.listTaskOperations)
			r.Get("/tasks/{id}", s.getTask)
			r.Get("/tasks/{id}/jobs", s.listJobs)
			r.Get("/tasks/{id}/events", s.listEvents)
			r.Get("/tasks/{id}/events/stream", s.streamEvents)
			r.Get("/tasks/{id}/activity", s.getTaskActivity)
			r.Get("/lineage/{type}/{id}", s.getLineage)
			r.With(s.requireMutationAuth).Post("/lineage/rebuild", s.rebuildLineage)
			r.Get("/tasks/{id}/interventions", s.listInterventions)
			r.Get("/tasks/{id}/spec", s.getLatestSpec)
			r.Get("/reviews", s.listReviews)
			r.Get("/workspace", s.getWorkspace)
			r.Get("/work-orders", s.listWorkOrders)
			r.Get("/monitor", s.getMonitorStatus)
			r.With(s.requireMutationAuth).Post("/work-orders/{id}/recover", s.recoverWorkOrder)
			r.Get("/blueprints", s.listBlueprints)
			r.Get("/requirements", s.listRequirements)
			r.With(s.requireMutationAuth).Post("/requirements", s.createRequirement)
			r.Get("/requirements/{id}", s.getRequirement)
			r.Get("/requirements/{id}/versions", s.listRequirementVersions)
			r.With(s.requireMutationAuth).Post("/requirements/{id}/versions", s.proposeRequirementVersion)
			r.With(s.requireMutationAuth).Post("/requirements/{id}/versions/{version}/confirm", s.confirmRequirementVersion)
			r.Get("/reference-documents", s.listReferenceDocuments)
			r.With(s.requireMutationAuth).Post("/reference-documents", s.createReferenceDocument)
			r.Get("/reference-documents/{id}/versions", s.listReferenceDocumentVersions)
			r.With(s.requireMutationAuth).Post("/reference-documents/{id}/versions", s.supersedeReferenceDocument)
			r.With(s.requireMutationAuth).Delete("/reference-documents/{id}", s.deleteReferenceDocument)
			r.Get("/system-designs", s.listSystemDesigns)
			r.With(s.requireMutationAuth).Post("/system-designs", s.createSystemDesign)
			r.Get("/system-designs/{id}", s.getSystemDesign)
			r.Get("/system-designs/{id}/versions", s.listSystemDesignVersions)
			r.With(s.requireMutationAuth).Post("/system-designs/{id}/versions", s.proposeSystemDesignVersion)
			r.With(s.requireMutationAuth).Post("/system-designs/{id}/versions/{version}/confirm", s.confirmSystemDesignVersion)
			r.Get("/decisions", s.listDecisions)
			r.With(s.requireMutationAuth).Post("/decisions", s.proposeDecision)
			r.Get("/decisions/{id}", s.getDecision)
			r.With(s.requireMutationAuth).Post("/decisions/{id}/confirm", s.confirmDecision)
			r.With(s.requireMutationAuth).Post("/decisions/{id}/dismiss", s.dismissDecision)
			r.Get("/planning-sessions", s.listPlanningSessions)
			r.With(s.requireMutationAuth).Post("/planning-sessions", s.createPlanningSession)
			r.Get("/planning-sessions/{id}", s.getPlanningSession)
			r.Get("/planning-sessions/{id}/messages", s.listPlanningMessages)
			r.With(s.requireMutationAuth).Post("/planning-sessions/{id}/messages", s.streamPlanningMessage)
			r.With(s.requireMutationAuth).Post("/planning-sessions/{id}/chat", s.streamPlanningMessage)
			r.With(s.requireMutationAuth).Post("/planning-sessions/{id}/abandon", s.abandonPlanningSession)
			r.Get("/planning-bundles", s.listPlanningBundles)
			r.Get("/planning-bundles/{id}", s.getPlanningBundle)
			r.With(s.requireMutationAuth).Post("/planning-bundles/{id}/approve", s.approvePlanningBundle)
			r.With(s.requireMutationAuth).Post("/planning-bundles/{id}/reject", s.rejectPlanningBundle)
			r.Get("/lifecycle-diagram", s.getLifecycleDiagram)
			r.With(s.requireMutationAuth).Get("/workspace/config", s.getWorkspaceConfig)
			r.With(s.requireMutationAuth).Put("/workspace/config", s.putWorkspaceConfig)
			r.With(s.requireMutationAuth).Post("/tasks", s.createTask)
			r.With(s.requireMutationAuth).Post("/monitor/observations", s.observeMonitorSignal)
			r.With(s.requireMutationAuth).Post("/monitor/drift/{id}/resolve", s.resolveMonitorDrift)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/redispatch", s.redispatchTask)
			r.With(s.requireMutationAuth).Put("/tasks/{id}/hold", s.setTaskHold)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/context", s.updateTaskContext)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/setup", s.changeTaskSetup)
			r.With(s.requireMutationAuth).Delete("/tasks/{id}/dependencies/{dependency_id}", s.removeTaskDependency)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/review-round/retry", s.retryReviewRound)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/review-round/recover", s.recoverInterruptedReviewRound)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/review", s.reviewTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/close", s.closeTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/merge", s.mergeTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/merge-conflict-fix", s.fixMergeConflict)
			r.With(s.requireMutationAuth).Get("/artifacts", s.listArtifacts)
			r.With(s.requireMutationAuth).Post("/artifacts", s.uploadArtifact)
			r.With(s.requireMutationAuth).Get("/artifacts/{id}", s.downloadArtifact)
			r.With(s.requireMutationAuth).Get("/workers", s.listWorkers)
			r.With(s.requireMutationAuth).Post("/workers/pairings", s.issueWorkerPairing)
			r.With(s.requireMutationAuth).Delete("/workers/{id}", s.revokeWorker)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
			// target (spec §3.3 T21).
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

func (s *Server) requireMutationAuth(next http.Handler) http.Handler {
	return s.requireBearerRole(core.ActorHuman, "local-operator", next)
}

func (s *Server) requireWorkspaceAuth(next http.Handler) http.Handler {
	// Unit-test and explicit memory stores have no durable workspace registry;
	// production multi-workspace reads follow the authenticated operator policy.
	if s.Workspaces == nil {
		return next
	}
	return s.requireMutationAuth(next)
}

func (s *Server) requireMCPAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.BearerToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(s.BearerToken)) == 1 {
			actorID := strings.TrimSpace(r.Header.Get("X-Conveyor-Actor"))
			if actorID == "" {
				actorID = "mcp-agent"
			}
			ctx := store.WithActor(r.Context(), store.Actor{ID: actorID, Role: core.ActorAgent})
			next.ServeHTTP(w, r.WithContext(ctx))
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

func (s *Server) requireBearerRole(role core.ActorRole, defaultID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.BearerToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.BearerToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		actorID := strings.TrimSpace(r.Header.Get("X-Conveyor-Actor"))
		if actorID == "" {
			actorID = defaultID
		}
		ctx := store.WithActor(r.Context(), store.Actor{ID: actorID, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	checkoutCommand, checkoutAvailable, checkoutGuidance, err := s.checkoutState(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if request.Action == core.InterventionPull && !checkoutAvailable {
		http.Error(w, checkoutGuidance, http.StatusConflict)
		return
	}
	latestJob, hasJob, err := s.Store.GetLatestJob(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	if err := s.Store.CreateIntervention(r.Context(), intervention); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIntervention == nil && request.Action == core.InterventionRedirect && s.OnCreate != nil {
		s.OnCreate(r.Context(), id)
	}
	updated, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	Mode            core.TaskMode        `json:"mode"` // deprecated §21.31: manual→hold, auto→no-op
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListJobs(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.ListEvents(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, interventions)
}

func (s *Server) getLatestSpec(w http.ResponseWriter, r *http.Request) {
	spec, ok, err := s.Store.GetLatestSpecVersion(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	LineageGraph              core.LineageTraversal                 `json:"lineage_graph"`
}

type activityItem struct {
	Task                      core.Task                             `json:"task"`
	LatestStage               core.Stage                            `json:"latest_stage,omitempty"`
	LastEventAt               time.Time                             `json:"last_event_at"`
	NeedsAttention            bool                                  `json:"needs_attention"`
	ForgeFailure              *store.ForgeFailure                   `json:"forge_failure,omitempty"`
	ReviewDiagnostics         []store.ReviewVerdictDiagnostic       `json:"review_diagnostics,omitempty"`
	ReviewRecovery            *store.ReviewRecoveryState            `json:"review_recovery,omitempty"`
	InterruptedReviewRecovery *store.InterruptedReviewRecoveryState `json:"interrupted_review_recovery,omitempty"`
	Stalled                   *store.StalledState                   `json:"stalled,omitempty"`
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	s.listActivityFiltered(w, r, true)
}

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	s.listActivityFiltered(w, r, false)
}

func (s *Server) listActivityFiltered(w http.ResponseWriter, r *http.Request, reviewsOnly bool) {
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markers, err := s.Store.ListActivityMarkers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	items := make([]activityItem, 0, len(tasks))
	for _, task := range tasks {
		// Blueprint anchors are intent artifacts, not claimable work, so they
		// leave the stage-grouped board and its counts for the Blueprints
		// surface (spec §21.49). The review inbox projection keeps them: an
		// anchor at its spec gate has not materialized children yet, so it is
		// not classified here and its approval card is untouched.
		if !reviewsOnly && core.BlueprintAnchor(task) {
			continue
		}
		marker := markerByTask[task.ID]
		if task.State == core.TaskMerged || task.State == core.TaskClosed {
			marker.Stalled = nil
		}
		if reviewsOnly && !reviewable(task.State) && marker.ForgeFailure == nil && marker.ReviewRecovery == nil && marker.InterruptedReviewRecovery == nil && marker.Stalled == nil {
			continue
		}
		items = append(items, activityItem{
			Task: task, LatestStage: marker.LatestStage, LastEventAt: marker.LastEventAt,
			NeedsAttention:            needsAttention(task, marker),
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
// work is waiting on a human (spec §13.3).
func needsAttention(task core.Task, marker store.ActivityMarker) bool {
	return task.State == core.TaskAwaiting || task.State == core.TaskParked ||
		marker.ForgeFailure != nil || marker.ReviewRecovery != nil ||
		marker.InterruptedReviewRecovery != nil || marker.Stalled != nil
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
	// (spec §21.58 change 3).
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

// taskOperationsItem is the list-first Tasks view's row (spec §21.58, REQ-1).
// Every field projects durable task, relationship, context, or plan authority;
// the view stores nothing and re-derives nothing of its own. No priority,
// assignee, or declared-phase field appears here, and none may be added
// (AC-1.5).
type taskOperationsItem struct {
	Task                 core.Task        `json:"task"`
	LatestStage          core.Stage       `json:"latest_stage,omitempty"`
	LastEventAt          time.Time        `json:"last_event_at"`
	NeedsAttention       bool             `json:"needs_attention"`
	UnsatisfiableTaskIDs []string         `json:"unsatisfiable_task_ids,omitempty"`
	ChildRollup          *taskChildRollup `json:"child_rollup,omitempty"`
	Plan                 taskPlanStatus   `json:"plan"`
}

// listTaskOperations serves the Phase 8.4 Tasks view (spec §21.58, REQ-1).
// Dependencies, blocking edges, and children come from the same task hydration
// the board and task detail already read; attached context is the existing
// append-only attachment fold; plan status is the persisted plan version plus
// the audited gate command. Per-task reads follow the blueprint-list
// precedent — the projection has no batch authority of its own to invent.
func (s *Server) listTaskOperations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markers, err := s.Store.ListActivityMarkers(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	taskIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	// Blocked stays a derived predicate owned by the dependency substrate
	// (spec §21.47); the view reads it, and reads the unsatisfiable edges the
	// task record itself does not carry.
	blockers, err := s.Store.ListDependencyBlockers(ctx, taskIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Attached documents repeat across rows; memoizing only the document reads
	// keeps the authoritative fold in store.TaskContextFromEvents.
	documents := &taskContextDocumentCache{Store: s.Store, requirements: map[string]core.Requirement{}, designs: map[string]core.SystemDesign{}}
	items := make([]taskOperationsItem, 0, len(tasks))
	for _, task := range tasks {
		events, eventsErr := s.Store.ListEvents(ctx, task.ID)
		if eventsErr != nil {
			http.Error(w, eventsErr.Error(), http.StatusInternalServerError)
			return
		}
		task.Context, err = store.TaskContextFromEvents(ctx, documents, events)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		latest, hasPlan, specErr := s.Store.GetLatestSpecVersion(ctx, task.ID)
		if specErr != nil {
			http.Error(w, specErr.Error(), http.StatusInternalServerError)
			return
		}
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		items = append(items, taskOperationsItem{
			Task:                 task,
			LatestStage:          marker.LatestStage,
			LastEventAt:          marker.LastEventAt,
			NeedsAttention:       needsAttention(task, marker),
			UnsatisfiableTaskIDs: blockers[task.ID].UnsatisfiableTaskIDs,
			ChildRollup:          taskChildRollupFor(task.Children),
			Plan:                 taskPlanStatusFor(latest, hasPlan, events),
		})
	}
	writeJSON(w, http.StatusOK, items)
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
// (spec §13.1, AC-1.4).
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	task.Context, err = store.TaskContextFromEvents(r.Context(), s.Store, events)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	interventions, err := s.Store.ListInterventions(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	spec, hasSpec, err := s.Store.GetLatestSpecVersion(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var specPointer *core.SpecVersion
	if hasSpec {
		spec.MaterializedChildren = materializedChildrenForSpec(task.Children, spec.Version)
		specPointer = &spec
	}
	workOrders, err := s.Store.ListTaskWorkOrders(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	// intake (spec §21.5); the task detail view previews them below the spec.
	// Conveyor-generated audit transcripts are deliberately excluded — they are
	// evidence, not attachments.
	attachments, err := s.taskAttachments(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verificationEvidence, err := s.taskVerificationEvidence(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	effectiveLineage, err := s.effectiveLineageBudget(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lineageGraph, err := s.lineageGraph(r, core.LineageNode{Type: core.LineageTask, ID: id}, core.LineageTraversalBudget{
		MaxDepth: effectiveLineage.Depth, MaxNodes: effectiveLineage.Nodes, MaxLinks: effectiveLineage.Links,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var workerStatus *workerservice.TaskWorkerStatus
	var mergeReadiness *dispatch.MergeReadiness
	if task.State == core.TaskApproved && s.OnMergeReadiness != nil {
		readiness, readinessErr := s.OnMergeReadiness(r.Context(), task)
		if readinessErr != nil {
			// The merge action must never render without a successful gate-facing
			// readiness read (spec §21.30 change 1).
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
	if s.Workers != nil && s.ConfigProvider != nil && !task.Hold && task.State != core.TaskMerged && task.State != core.TaskClosed {
		if cfg, cfgErr := s.ConfigProvider(r.Context()); cfgErr == nil {
			workerStatus = s.Workers.TaskAvailability(r.Context(), cfg, task, workOrders)
		}
	}
	stalled := store.StalledTask(workOrders)
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
		stalled = nil
	}
	writeJSON(w, http.StatusOK, reviewItem{
		Task: task, Jobs: jobs, Events: events, Interventions: interventions,
		CheckoutCommand: checkoutCommand, CheckoutAvailable: checkoutAvailable, CheckoutGuidance: checkoutGuidance,
		NeedsAttention:            task.State == core.TaskAwaiting || task.State == core.TaskParked || store.LatestForgeFailure(events) != nil || store.ReviewRecoveryNeeded(workOrders, events) != nil || store.InterruptedReviewRecoveryNeeded(workOrders) != nil || stalled != nil,
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
		LineageGraph:              lineageGraph,
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
			message = message[:120] + "…"
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

func (s *Server) checkoutState(ctx context.Context, taskID string) (string, bool, string, error) {
	command, available, guidance := checkoutStateFromHistory(taskID, nil)
	return command, available, guidance, nil
}

func checkoutStateFromHistory(taskID string, _ []core.Event) (string, bool, string) {
	// The checkout helper can safely create a missing assigned branch from the
	// freshly fetched base, so the dedicated-worktree command is available as
	// soon as the task exists (spec §21.8).
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
