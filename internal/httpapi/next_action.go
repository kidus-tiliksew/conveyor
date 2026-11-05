package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) writeTask(w http.ResponseWriter, r *http.Request, task core.Task) {
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	action, err := store.ReadNextAction(r.Context(), s.Store, task.ID, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		core.Task
		NextAction nextaction.NextAction `json:"next_action"`
	}{Task: task, NextAction: action})
}

func (s *Server) writeTasks(w http.ResponseWriter, r *http.Request, tasks []core.Task) {
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
