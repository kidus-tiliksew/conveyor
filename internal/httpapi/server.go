// Package httpapi is the control-plane REST + SSE surface (spec §17.3).
// Phase 1 exposes task CRUD and job log streaming; review actions,
// runner registration, and webhook ingestion land in later phases. All
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
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type Server struct {
	Store store.Store
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
		// TODO(phase1): GET /v1/jobs/{id}/logs — SSE stream from the
		// runner's StreamLogs (spec §17.3, Phase 1 "logs only").
	})
	return r
}

type createTaskReq struct {
	Workspace  string `json:"workspace"`
	Title      string `json:"title"`
	BaseBranch string `json:"base_branch"`
	Source     string `json:"source"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if req.Source == "" {
		req.Source = "api"
	}
	id := newTaskID()
	t := core.Task{
		ID:         id,
		Workspace:  req.Workspace,
		Source:     req.Source,
		Title:      req.Title,
		Level:      core.L2,
		BaseBranch: req.BaseBranch,
		Branch:     "conveyor/task-" + id,
		State:      core.TaskQueued,
		CreatedAt:  time.Now(),
	}
	if err := s.Store.CreateTask(t); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newTaskID() string {
	// Short, human-typeable IDs; collision-checked by CreateTask.
	return time.Now().UTC().Format("060102-150405")
}
