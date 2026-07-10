// Package httpapi is the control-plane REST + SSE surface (spec §17.3).
// Phase 1 exposes task CRUD and job listing; review actions, runner
// registration, and webhook ingestion land in later phases. All
// mutating endpoints will be recorded in the events table once Phase 2
// introduces it.
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type Server struct {
	Store store.Store
	// Repos is the set of valid repo names; nil skips validation.
	Repos []string
	// OnCreate is invoked with each created task's ID (the dispatcher's
	// Enqueue). Nil means tasks queue without dispatch (tests).
	OnCreate func(taskID string)
	// Workspace stamps created tasks.
	Workspace string
}

func NewServer(s store.Store) *Server { return &Server{Store: s} }

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Get("/tasks", s.listTasks)
		r.Post("/tasks", s.createTask)
		r.Get("/tasks/{id}", s.getTask)
		r.Get("/tasks/{id}/jobs", s.listJobs)
		// TODO(phase1-followup): GET /v1/jobs/{id}/logs — SSE stream
		// (spec §17.3); Phase 1 logs land in the control dir and
		// conveyord stdout.
	})
	return r
}

type createTaskReq struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	Repo       string `json:"repo"`
	BaseBranch string `json:"base_branch"`
	Source     string `json:"source"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if s.Repos != nil && !contains(s.Repos, req.Repo) {
		http.Error(w, "unknown repo "+req.Repo, http.StatusBadRequest)
		return
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if req.Source == "" {
		req.Source = "api"
	}
	id := core.NewTaskID()
	t := core.Task{
		ID:         id,
		Workspace:  s.Workspace,
		Source:     req.Source,
		Title:      req.Title,
		Body:       req.Body,
		Level:      core.L2,
		Repo:       req.Repo,
		BaseBranch: req.BaseBranch,
		Branch:     gitx.BranchName(id),
		State:      core.TaskQueued,
		CreatedAt:  time.Now(),
	}
	if err := s.Store.CreateTask(t); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if s.OnCreate != nil {
		s.OnCreate(id)
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := s.Store.ListTasks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetTask(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListJobs(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
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
