package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) changeTaskPolicy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		VerifyStage          *bool             `json:"verify_stage"`
		StageTimeouts        map[string]string `json:"stage_timeouts"`
		ReviewDistinctModels *bool             `json:"review_distinct_models"`
		Reason               string            `json:"reason"`
		RequestID            string            `json:"request_id"`
	}
	body, err := readBoundedBody(w, r, 16384)
	if errors.Is(err, errBodyTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one policy object"})
		return
	}
	policy := store.TaskPolicyChange{VerifyStage: input.VerifyStage, StageTimeouts: input.StageTimeouts, ReviewDistinctModels: input.ReviewDistinctModels}
	if err := policy.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(input.Reason) == "" || strings.TrimSpace(input.RequestID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nonblank reason and request_id are required"})
		return
	}
	result, err := s.WorkOrders.ChangeTaskPolicy(r.Context(), chi.URLParam(r, "id"), input.Reason, input.RequestID, policy)
	if err != nil {
		if !isOperationRefusal(err) {
			writeStoreFailure(w, r, err)
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrSetupChangeConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
