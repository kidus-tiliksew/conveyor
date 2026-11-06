package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// getTaskAudit shares the human workspace read boundary with task detail.
// Validate task ownership before looking up a selector (component-http-api,
// DEC-19). Public serializers continue to exclude private claim credentials.
func (s *Server) getTaskAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	taskID := chi.URLParam(r, "id")
	workspace, scoped := store.WorkspaceFromContext(r.Context())
	task, err := s.Store.GetTask(r.Context(), taskID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeStoreFailure(w, r, err)
		return
	}
	// A missing task and a task in another workspace answer one not-found
	// (req-accounts-and-membership AC-4.2).
	if err != nil || !scoped || task.Workspace != workspace {
		http.NotFound(w, r)
		return
	}
	kind, recordID := chi.URLParam(r, "kind"), chi.URLParam(r, "record_id")
	switch kind {
	case "event":
		eventID, err := strconv.ParseInt(recordID, 10, 64)
		if err != nil || eventID <= 0 || strconv.FormatInt(eventID, 10) != recordID {
			http.Error(w, "invalid audit selector", http.StatusBadRequest)
			return
		}
		events, err := s.Store.ListEvents(r.Context(), taskID)
		if err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		for _, event := range events {
			if event.ID == eventID && event.TaskID == taskID {
				presented := []core.Event{event}
				if err := s.annotateEventActorOwners(r.Context(), presented); err != nil {
					writeStoreFailure(w, r, err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "event": presented[0]})
				return
			}
		}
	case "work-order":
		order, err := s.Store.GetWorkOrder(r.Context(), recordID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeStoreFailure(w, r, err)
			return
		}
		if err == nil && order.TaskID == taskID {
			writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "work_order": order})
			return
		}
	default:
		http.Error(w, "invalid audit selector", http.StatusBadRequest)
		return
	}
	http.NotFound(w, r)
}
