package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) writeTask(w http.ResponseWriter, r *http.Request, task core.Task) {
	if response, ok := s.taskResponse(w, r, task); ok {
		writeJSON(w, http.StatusOK, response)
	}
}

// taskResponse is a task with its next action. On a failed read it answers the
// request itself and reports false.
func (s *Server) taskResponse(w http.ResponseWriter, r *http.Request, task core.Task) (any, bool) {
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	action, err := store.ReadNextAction(r.Context(), s.Store, task.ID, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	return struct {
		core.Task
		NextAction nextaction.NextAction `json:"next_action"`
	}{Task: task, NextAction: action}, true
}

// writeTasks answers one task page with each row's compact next action. The
// paging headers go out only with the rows, so a failed read states no page.
func (s *Server) writeTasks(w http.ResponseWriter, r *http.Request, page store.TaskPage, limit, offset int) {
	tasks := page.Tasks
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// Rows carry only the compact action, so the projection skips the
	// disposition read and covers the given page alone (component-http-api,
	// Task list read).
	opts.CompactOnly = true
	projected, err := store.ReadNextActions(r.Context(), s.Store, tasks, opts)
	if err != nil {
		// A row without its next action would read as a task waiting on
		// nothing, so the read fails (req-operator-next-action AC-1.7;
		// component-next-action invariant 1; component-http-api invariant 16).
		logged, _ := redact.New(nil).Redact(err.Error())
		log.Printf("task list next action: request_id=%q error=%q", middleware.GetReqID(r.Context()), logged)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(offset))
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) nextActionOptions(r *http.Request) (store.NextActionOptions, error) {
	return s.nextActionOptionsFor(r.Context())
}

// nextActionCapabilities are the capabilities that gate next-action choices.
var nextActionCapabilities = []core.Capability{core.CapabilityOperateGates, core.CapabilityRecoverWork, core.CapabilityRequestChanges, core.CapabilityConfirmDocuments}

// nextActionOptionsFor derives projection options from the request
// credential, so REST routes and MCP reads state one next action (DEC-61).
// Each capability is granted exactly when route admission would grant it:
// the caller's workspace role when memberships are configured, otherwise
// the operator credential scope.
func (s *Server) nextActionOptionsFor(ctx context.Context) (store.NextActionOptions, error) {
	opts := store.NextActionOptions{Now: time.Now().UTC(), Capabilities: map[core.Capability]bool{}}
	for _, capability := range nextActionCapabilities {
		allowed, err := s.callerCapability(ctx, capability)
		if err != nil {
			return store.NextActionOptions{}, err
		}
		if allowed {
			opts.Capabilities[capability] = true
		}
	}
	opts.MergeReadiness = s.mergeReadiness.lastKnown
	return opts, nil
}

func (s *Server) getNextAction(w http.ResponseWriter, r *http.Request) {
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	action, err := store.ReadNextAction(r.Context(), s.Store, chi.URLParam(r, "id"), opts)
	if err != nil {
		writeClassifiedStoreError(w, r, err, refuseSentinel(store.ErrNotFound, http.StatusNotFound, "task not found"))
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
	listed, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListCallerAttentionTaskPage(r.Context(), store.CallerAttentionQuery{UserID: credential.OwnerUserID, Limit: listed.Limit, Offset: listed.Offset})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	projected, err := store.ReadNextActions(r.Context(), s.Store, page.Tasks, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	items := make([]nextaction.Compact, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		items = append(items, nextaction.CompactFrom(projected[task.ID]))
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": page.Total})
}

// listFactoryActs serves the workspace's system recovery acts, newest first,
// from one bounded store page (DEC-105(4); component-next-action invariant 21).
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
	listed, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListRecoveryActPage(r.Context(), store.RecoveryActQuery{Since: since, Page: listed.query()})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, map[string]any{"acts": page.Acts})
}
