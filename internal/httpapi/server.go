// Package httpapi is the Phase 2 control-plane REST + SSE surface and embedded
// activity/review SPA (spec §13.3, §17.3). Mutations are authenticated and
// recorded with actor identity in the append-only event stream.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type Server struct {
	Store store.Store
	// Repos is the set of valid repo names; nil skips validation.
	Repos []string
	// OnCreate is invoked with each created task's ID (the dispatcher's
	// Enqueue). Nil means tasks queue without dispatch (tests).
	OnCreate func(context.Context, string)
	// OnIntervention advances stage gates after the append-only decision is
	// committed (spec §4, §13.2).
	OnIntervention func(context.Context, core.Task, core.Job, core.Intervention) error
	// OnMerge performs and authoritatively confirms the final forge merge.
	OnMerge func(context.Context, core.Task) error
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
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/renew", s.renewWorkerOrder)
		r.With(s.requireWorkerAuth).Post("/worker/work-orders/{id}/release", s.releaseWorkerOrder)
		r.With(s.requireMutationAuth).Get("/workspaces", s.listWorkspaces)
		r.With(s.requireMutationAuth).Post("/workspaces", s.createWorkspace)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Get("/workspaces/{workspace_id}", s.getWorkspaceRecord)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Get("/workspaces/{workspace_id}/config", s.getWorkspaceConfig)
		r.With(s.requireMutationAuth, s.resolveWorkspaceContext).Put("/workspaces/{workspace_id}/config", s.putWorkspaceConfig)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorkspaceAuth, s.resolveWorkspaceContext)
			r.Get("/activity", s.listActivity)
			r.Get("/tasks", s.listTasks)
			r.Get("/tasks/{id}", s.getTask)
			r.Get("/tasks/{id}/jobs", s.listJobs)
			r.Get("/tasks/{id}/events", s.listEvents)
			r.Get("/tasks/{id}/events/stream", s.streamEvents)
			r.Get("/tasks/{id}/activity", s.getTaskActivity)
			r.Get("/tasks/{id}/interventions", s.listInterventions)
			r.Get("/tasks/{id}/spec", s.getLatestSpec)
			r.Get("/reviews", s.listReviews)
			r.Get("/workspace", s.getWorkspace)
			r.Get("/work-orders", s.listWorkOrders)
			r.Get("/requirements", s.listRequirements)
			r.With(s.requireMutationAuth).Get("/workspace/config", s.getWorkspaceConfig)
			r.With(s.requireMutationAuth).Put("/workspace/config", s.putWorkspaceConfig)
			r.With(s.requireMutationAuth).Post("/tasks", s.createTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/redispatch", s.redispatchTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/review", s.reviewTask)
			r.With(s.requireMutationAuth).Post("/tasks/{id}/merge", s.mergeTask)
			r.With(s.requireMutationAuth).Post("/features", s.createFeature)
			r.With(s.requireMutationAuth).Put("/tasks/{id}/feature", s.assignTaskFeature)
			r.With(s.requireMutationAuth).Get("/artifacts", s.listArtifacts)
			r.With(s.requireMutationAuth).Post("/artifacts", s.uploadArtifact)
			r.With(s.requireMutationAuth).Get("/artifacts/{id}", s.downloadArtifact)
			r.With(s.requireMutationAuth).Get("/workers", s.listWorkers)
			r.With(s.requireMutationAuth).Post("/workers/pairings", s.issueWorkerPairing)
			r.With(s.requireMutationAuth).Delete("/workers/{id}", s.revokeWorker)
		})
	})
	r.With(s.requireMCPAuth).Post("/mcp", s.handleMCP)
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
		if t.NextStage == "" {
			http.Error(w, "task has no decided next stage; record an explicit review redirect instead", http.StatusConflict)
			return
		}
		if err := s.Store.UpdateTaskState(r.Context(), id, core.TaskQueued); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		t.State = core.TaskQueued
	}
	if s.OnCreate != nil {
		s.OnCreate(r.Context(), id)
	}
	writeJSON(w, http.StatusAccepted, t)
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
	if !request.Action.Valid() {
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
	if err := s.Store.CreateIntervention(r.Context(), intervention); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIntervention != nil {
		if err := s.OnIntervention(r.Context(), task, latestJob, intervention); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if request.Action == core.InterventionRedirect && s.OnCreate != nil {
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

func reviewable(state core.TaskState) bool {
	return state == core.TaskAwaiting || state == core.TaskParked || state == core.TaskApproved
}

type createTaskReq struct {
	Title         string               `json:"title"`
	Body          string               `json:"body"`
	Repo          string               `json:"repo"`
	BaseBranch    string               `json:"base_branch"`
	Source        string               `json:"source"`
	Level         core.EscalationLevel `json:"level"`
	Mode          core.TaskMode        `json:"mode"`
	SpecApproval  *bool                `json:"spec_approval,omitempty"`
	MergeApproval *bool                `json:"merge_approval,omitempty"`
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
		http.Error(w, err.Error(), taskCreateStatus(err))
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
		http.Error(w, err.Error(), taskCreateStatus(err))
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
	if err = s.Store.UpdateTaskState(r.Context(), result.Task.ID, core.TaskQueued); err != nil {
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
	writeJSON(w, http.StatusOK, spec)
}

type reviewItem struct {
	Task              core.Task                       `json:"task"`
	Jobs              []core.Job                      `json:"jobs"`
	Events            []core.Event                    `json:"events"`
	Interventions     []core.Intervention             `json:"interventions"`
	CheckoutCommand   string                          `json:"checkout_command,omitempty"`
	CheckoutAvailable bool                            `json:"checkout_available"`
	CheckoutGuidance  string                          `json:"checkout_guidance"`
	NeedsAttention    bool                            `json:"needs_attention"`
	Spec              *core.SpecVersion               `json:"spec,omitempty"`
	WorkOrders        []core.WorkOrder                `json:"work_orders"`
	ReviewDiagnostics []store.ReviewVerdictDiagnostic `json:"review_diagnostics,omitempty"`
}

type activityItem struct {
	Task              core.Task                       `json:"task"`
	LatestStage       core.Stage                      `json:"latest_stage,omitempty"`
	LastEventAt       time.Time                       `json:"last_event_at"`
	NeedsAttention    bool                            `json:"needs_attention"`
	ReviewDiagnostics []store.ReviewVerdictDiagnostic `json:"review_diagnostics,omitempty"`
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
		if reviewsOnly && !reviewable(task.State) {
			continue
		}
		marker := markerByTask[task.ID]
		items = append(items, activityItem{
			Task: task, LatestStage: marker.LatestStage, LastEventAt: marker.LastEventAt,
			NeedsAttention:    task.State == core.TaskAwaiting || task.State == core.TaskParked,
			ReviewDiagnostics: marker.ReviewDiagnostics,
		})
	}
	writeJSON(w, http.StatusOK, items)
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
		specPointer = &spec
	}
	workOrders, err := s.Store.ListTaskWorkOrders(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	checkoutCommand, checkoutAvailable, checkoutGuidance := checkoutStateFromHistory(id, events)
	writeJSON(w, http.StatusOK, reviewItem{
		Task: task, Jobs: jobs, Events: events, Interventions: interventions,
		CheckoutCommand: checkoutCommand, CheckoutAvailable: checkoutAvailable, CheckoutGuidance: checkoutGuidance,
		NeedsAttention:    task.State == core.TaskAwaiting || task.State == core.TaskParked,
		Spec:              specPointer,
		WorkOrders:        workOrders,
		ReviewDiagnostics: store.ReviewVerdictDiagnostics(workOrders, events, time.Now().UTC()),
	})
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
