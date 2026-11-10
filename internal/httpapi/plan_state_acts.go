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
	plan, err := pipeline.ParsePlan(*input.Plan, nil)
	if err != nil {
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
	result, err := s.attachPlan(r.Context(), request, plan)
	if err != nil {
		var riskErr *attachPlanRiskError
		if errors.As(err, &riskErr) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plan: " + riskErr.Error()})
			return
		}
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

// attachPlanRiskError refuses an attached plan that states no risk tier on a
// task whose frozen risk mode is enforce. The route answers 400 and records
// nothing (req-risk-based-review AC-4.2; feature-risk-based-review RR-4).
type attachPlanRiskError struct{ err error }

func (e *attachPlanRiskError) Error() string { return e.err.Error() }
func (e *attachPlanRiskError) Unwrap() error { return e.err }

// attachPlan runs the attach under the task side-effect lock and the
// frozen-policy exception span the /policy route shares, so a claim, a
// policy change, and the attach serialize on the task
// (component-task-lifecycle invariant 44). Under the same lock, before the
// command can record a version, cancel an order, or record a replay, it reads
// the task's frozen risk policy and refuses a plan with no tier under enforce
// (req-risk-based-review AC-4.2). A task the read does not find goes on to the
// command, which answers it as before.
func (s *Server) attachPlan(ctx context.Context, request store.AttachPlanRequest, plan pipeline.Spec) (store.AttachPlanResult, error) {
	var result store.AttachPlanResult
	err := s.Store.WithTaskSideEffectLock(ctx, request.TaskID, func(ctx context.Context) error {
		task, getErr := s.Store.GetTask(ctx, request.TaskID)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
		if getErr == nil {
			if riskErr := pipeline.CheckPlanRiskTier(task.SetupContract.Risk, plan); riskErr != nil {
				return &attachPlanRiskError{err: riskErr}
			}
		}
		var attachErr error
		result, attachErr = taskops.ExecuteSetupChange(ctx, s.Store, request.TaskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return s.Store.AttachTaskPlanCommand(ctx, lease, request)
		})
		return attachErr
	})
	return result, err
}

// withPlanRisk fills a served plan version's risk tier and reason from its
// own content with the shared plan grammar, so the plan card shows the tier
// the version states. A version that states none, or an earlier version
// whose content does not parse, is served with no tier rather than an
// invented one (req-risk-based-review AC-4.4; feature-risk-based-review RR-4).
func withPlanRisk(spec core.SpecVersion) core.SpecVersion {
	spec.RiskTier, spec.RiskReason = pipeline.PlanRiskOf(spec.Content)
	return spec
}
