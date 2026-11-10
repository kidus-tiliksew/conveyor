package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// attachTaskPlan is POST /v1/tasks/{id}/plan under operate_gates. Its strict
// body is {plan, base_commit, reason, request_id}, base_commit optional. The
// plan passes the content rules of a submitted plan and base_commit is a full
// commit ID, as at intake; each refusal answers 400 and records nothing. The
// backend applies component-plan-state invariant 5's predicate, answering
// 409, and records the next plan version, cancels the orders the act
// retires, and moves the plan state to awaiting_approval in one transaction.
// Approval comes only through the plan_approval gate (req-task-plan-state
// AC-3.2; DEC-125(6)).
func (s *Server) attachTaskPlan(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Plan       *string `json:"plan"`
		BaseCommit *string `json:"base_commit"`
		Reason     string  `json:"reason"`
		RequestID  string  `json:"request_id"`
	}
	body, err := readBoundedBody(w, r, taskCreateBodyLimit)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one plan object"})
		return
	}
	if strings.TrimSpace(input.Reason) == "" || strings.TrimSpace(input.RequestID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nonblank reason and request_id are required"})
		return
	}
	if input.Plan == nil || strings.TrimSpace(*input.Plan) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plan is required"})
		return
	}
	if _, err := pipeline.ParsePlan(*input.Plan, nil); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plan: " + err.Error()})
		return
	}
	request := store.AttachPlanRequest{TaskID: chi.URLParam(r, "id"), RequestID: input.RequestID, Reason: input.Reason, Plan: *input.Plan}
	if input.BaseCommit != nil {
		if !core.ValidPlanBaseCommit(*input.BaseCommit) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_commit must be a full commit ID of 40 lowercase hex characters"})
			return
		}
		request.BaseCommit = *input.BaseCommit
	}
	result, err := s.attachPlan(r.Context(), request)
	if err != nil {
		if !isOperationRefusal(err) {
			writeStoreFailure(w, r, err)
			return
		}
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, store.ErrSetupChangeConflict):
			status = http.StatusConflict
		case errors.Is(err, store.ErrNotFound):
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// attachPlan runs the attach under the task side-effect lock and the
// frozen-policy exception span the /policy route shares, so a claim, a
// policy change, and the attach serialize on the task
// (component-task-lifecycle invariant 44).
func (s *Server) attachPlan(ctx context.Context, request store.AttachPlanRequest) (store.AttachPlanResult, error) {
	var result store.AttachPlanResult
	err := s.Store.WithTaskSideEffectLock(ctx, request.TaskID, func(ctx context.Context) error {
		var attachErr error
		result, attachErr = taskops.ExecuteSetupChange(ctx, s.Store, request.TaskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return s.Store.AttachTaskPlanCommand(ctx, lease, request)
		})
		return attachErr
	})
	return result, err
}
