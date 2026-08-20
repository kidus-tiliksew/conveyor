package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
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
			log.Printf("update task context: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, context)
}

func (s *Server) confirmTaskContextProposal(w http.ResponseWriter, r *http.Request) {
	proposal, err := s.Store.ConfirmTaskContextProposal(r.Context(), chi.URLParam(r, "id"), core.TaskContextProposalTargetKind(chi.URLParam(r, "kind")), chi.URLParam(r, "target_id"))
	if err != nil {
		writeTaskContextProposalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) dismissTaskContextProposal(w http.ResponseWriter, r *http.Request) {
	proposal, err := s.Store.DismissTaskContextProposal(r.Context(), chi.URLParam(r, "id"), core.TaskContextProposalTargetKind(chi.URLParam(r, "kind")), chi.URLParam(r, "target_id"))
	if err != nil {
		writeTaskContextProposalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func writeTaskContextProposalError(w http.ResponseWriter, err error) {
	var referenceErr *store.TaskContextReferenceError
	switch {
	case errors.As(err, &referenceErr):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_context_reference", "message": referenceErr.Error()})
	case errors.Is(err, store.ErrTaskTerminal):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_terminal", "message": "task context proposals can only be decided while the task is open"})
	case errors.Is(err, store.ErrTaskContextProposalTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_proposal_transition", "message": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		log.Printf("decide task context proposal: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
