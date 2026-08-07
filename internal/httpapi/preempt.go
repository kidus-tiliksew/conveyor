package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) preemptWorkOrder(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Reason    string `json:"reason"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.WorkOrders.Preempt(r.Context(), chi.URLParam(r, "id"), request.Reason, request.RequestID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrWorkOrderPreemptConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
