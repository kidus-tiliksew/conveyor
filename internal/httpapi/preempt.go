package httpapi

import (
	"errors"
	"net/http"
	"strings"

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
	if err := decodeOptionalBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	headerRequestID := strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
	if request.RequestID == "" {
		request.RequestID = headerRequestID
	} else if headerRequestID != "" && strings.TrimSpace(request.RequestID) != headerRequestID {
		http.Error(w, "request_id and X-Idempotency-Key must match", http.StatusBadRequest)
		return
	}
	result, err := s.WorkOrders.Preempt(r.Context(), chi.URLParam(r, "id"), request.Reason, request.RequestID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrWorkOrderPreemptConflict) {
			status = http.StatusConflict
		}
		writeOperationError(w, r, err, status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
