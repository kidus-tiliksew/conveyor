package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) writeTask(w http.ResponseWriter, r *http.Request, task core.Task) {
	action, err := store.ReadNextAction(r.Context(), s.Store, task.ID, s.nextActionOptions(r))
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		core.Task
		NextAction nextaction.NextAction `json:"next_action"`
	}{Task: task, NextAction: action})
}

func (s *Server) writeTasks(w http.ResponseWriter, r *http.Request, tasks []core.Task) {
	projected, err := store.ReadNextActions(r.Context(), s.Store, tasks, s.nextActionOptions(r))
	if err != nil {
		writeJSON(w, http.StatusOK, tasks)
		return
	}
	rows := make([]struct {
		core.Task
		NextAction nextaction.Compact `json:"next_action"`
	}, len(tasks))
	for i, task := range tasks {
		rows[i].Task = task
		rows[i].NextAction = nextaction.CompactFrom(projected[task.ID])
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) nextActionOptions(r *http.Request) store.NextActionOptions {
	opts := store.NextActionOptions{Now: time.Now().UTC(), Capabilities: map[core.Capability]bool{}}
	credential, ok := store.CredentialFromContext(r.Context())
	if ok && credential.Kind == core.CredentialUser && credential.Scope == core.CredentialScopeOperator && s.Memberships == nil {
		for _, capability := range []core.Capability{core.CapabilityOperateGates, core.CapabilityRecoverWork, core.CapabilityRequestChanges, core.CapabilityConfirmDocuments} {
			opts.Capabilities[capability] = true
		}
	}
	return opts
}

func (s *Server) getNextAction(w http.ResponseWriter, r *http.Request) {
	action, err := store.ReadNextAction(r.Context(), s.Store, chi.URLParam(r, "id"), s.nextActionOptions(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, action)
}

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request) {
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.OwnerUserID == "" {
		http.Error(w, "authenticated user is required", http.StatusUnauthorized)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	page, err := s.Store.ListCallerAttentionTaskPage(r.Context(), store.CallerAttentionQuery{UserID: credential.OwnerUserID, Limit: limit, Offset: offset})
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	projected, err := store.ReadNextActions(r.Context(), s.Store, page.Tasks, s.nextActionOptions(r))
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	items := make([]nextaction.Compact, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		items = append(items, nextaction.CompactFrom(projected[task.ID]))
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": page.Total})
}

func (s *Server) listFactoryActs(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var events []core.Event
	for _, task := range tasks {
		taskEvents, listErr := s.Store.ListEvents(r.Context(), task.ID)
		if listErr != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		events = append(events, taskEvents...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"acts": store.FilterFactoryActs(events, since, limit)})
}
