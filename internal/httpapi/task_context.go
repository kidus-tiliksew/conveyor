package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) updateTaskContext(w http.ResponseWriter, r *http.Request) {
	var request store.TaskContextChange
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	context, err := s.Store.UpdateTaskContext(r.Context(), chi.URLParam(r, "id"), request)
	if err != nil {
		var referenceErr *store.TaskContextReferenceError
		switch {
		case errors.As(err, &referenceErr):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_context_reference", "message": referenceErr.Error()})
		case errors.Is(err, store.ErrTaskTerminal):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "task_terminal", "message": "task context can only be changed while the task is open"})
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, context)
}
