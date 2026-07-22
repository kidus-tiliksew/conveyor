package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) changeTaskSetup(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Setup       string `json:"setup"`
		ApplyLatest bool   `json:"apply_latest"`
		Reason      string `json:"reason"`
		RequestID   string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.ApplyLatest {
		if strings.TrimSpace(request.Setup) != "" {
			http.Error(w, "apply_latest and setup are mutually exclusive", http.StatusBadRequest)
			return
		}
		task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		request.Setup = task.SetupName
	}
	result, err := s.WorkOrders.ChangeTaskSetup(r.Context(), chi.URLParam(r, "id"), request.Setup, request.Reason, request.RequestID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrSetupChangeConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
