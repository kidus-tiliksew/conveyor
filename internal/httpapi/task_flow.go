package httpapi

import (
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// getTaskFlow serves GET /v1/tasks/{id}/flow: the task's stage path folded
// from its events with the NA-3 next action embedded (feature-operator-next-action NA-7).
// A task outside the request's workspace is not found, as with task audit.
func (s *Server) getTaskFlow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	workspace, scoped := store.WorkspaceFromContext(r.Context())
	task, err := s.Store.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (!scoped || task.Workspace != workspace) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("task flow %s: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id)
	if err != nil {
		log.Printf("task flow events %s: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The one-task batch is exactly ReadNextAction without a second task read.
	opts, err := s.nextActionOptions(r)
	if err != nil {
		log.Printf("task flow next action %s: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	projected, err := store.ReadNextActions(r.Context(), s.Store, []core.Task{task}, opts)
	if err != nil {
		log.Printf("task flow next action %s: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	action := projected[id]
	recovery, err := s.recoveryPolicy(r)
	if err != nil {
		log.Printf("task flow recovery policy %s: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Flow store.TaskFlow `json:"flow"`
	}{Flow: store.FoldTaskFlow(events, action, recovery)})
}

// recoveryPolicy reads the workspace's automatic-recovery bound, defaulting
// as the clock does when the workspace stores no document.
func (s *Server) recoveryPolicy(r *http.Request) (config.RecoveryPolicy, error) {
	if s.ConfigStore == nil {
		return config.DefaultRecoveryPolicy(), nil
	}
	record, err := s.ConfigStore.WorkspaceConfig(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		return config.DefaultRecoveryPolicy(), nil
	}
	if err != nil {
		return config.RecoveryPolicy{}, err
	}
	policy := record.Document.Recovery
	if err := policy.Normalize(); err != nil {
		return config.RecoveryPolicy{}, err
	}
	return policy, nil
}
