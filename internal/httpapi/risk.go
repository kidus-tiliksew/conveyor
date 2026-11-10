package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskRisk reads the task read field risk that REST, MCP, the CLI, and the
// task page share (req-risk-based-review REQ-11, AC-11.2;
// component-risk-review, Public surfaces). A task whose frozen risk mode is
// off or absent has no risk field and costs no read (AC-1.1).
func (s *Server) taskRisk(ctx context.Context, task core.Task) (*core.TaskRisk, error) {
	if !task.SetupContract.Risk.Enabled() {
		return nil, nil
	}
	scoped := store.WithWorkspace(ctx, task.Workspace)
	ratings, err := s.Store.ListTaskRiskRatings(scoped, task.ID)
	if err != nil {
		return nil, fmt.Errorf("list risk ratings: %w", err)
	}
	// Like the review seat projection, a task that froze no seat list takes
	// the workspace seat count.
	workspaceSeats := 0
	if task.SetupContract.Review.Seats == nil && s.ConfigProvider != nil {
		cfg, cfgErr := s.ConfigProvider(scoped)
		if cfgErr != nil {
			return nil, fmt.Errorf("resolve review seat minimum: %w", cfgErr)
		}
		if cfg != nil {
			workspaceSeats = len(cfg.Review.Seats)
		}
	}
	return store.ProjectTaskRisk(task, ratings, store.ReviewSeatMinimum(task.SetupContract, workspaceSeats)), nil
}

// riskTierRequest is the body of POST /v1/tasks/{id}/risk-tier.
type riskTierRequest struct {
	Tier   string `json:"tier"`
	Reason string `json:"reason"`
}

// decodeRiskTierRequest strictly decodes exactly one {tier, reason} object.
// Unknown fields, trailing JSON, a non-string value, and null are refused, and
// the tier must be low, medium, or high with one non-blank line of reason.
func decodeRiskTierRequest(body io.Reader) (riskTierRequest, error) {
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(io.LimitReader(body, 64<<10))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return riskTierRequest{}, errors.New("body must be one {tier, reason} JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return riskTierRequest{}, errors.New("body must be one {tier, reason} JSON object")
	}
	var request riskTierRequest
	for key, value := range raw {
		var target *string
		switch key {
		case "tier":
			target = &request.Tier
		case "reason":
			target = &request.Reason
		default:
			return riskTierRequest{}, fmt.Errorf("unknown field %q; the body holds only tier and reason", key)
		}
		if len(value) == 0 || value[0] != '"' {
			return riskTierRequest{}, fmt.Errorf("%s must be a string", key)
		}
		if err := json.Unmarshal(value, target); err != nil {
			return riskTierRequest{}, fmt.Errorf("%s must be a string", key)
		}
	}
	if _, ok := raw["tier"]; !ok {
		return riskTierRequest{}, errors.New("tier is required")
	}
	if _, err := riskpolicy.ParseTier(request.Tier); err != nil {
		return riskTierRequest{}, errors.New("tier must be low, medium, or high")
	}
	if err := core.ValidateRiskReason("reason", request.Reason); err != nil {
		return riskTierRequest{}, err
	}
	return request, nil
}

// raiseTaskRiskTier serves POST /v1/tasks/{id}/risk-tier under operate_gates.
// It records the authenticated person's raise only when the tier is above the
// task's current tier; a tier at or below it is refused with the current tier
// named and nothing recorded. No route lowers a tier, and the raise changes
// no stage, plan state, queue position, claim eligibility, seat, gate, or
// merge decision (req-risk-based-review REQ-6, AC-6.1, AC-6.2, AC-1.6, AC-2.2,
// AC-2.3; feature-risk-based-review RR-8).
func (s *Server) raiseTaskRiskTier(w http.ResponseWriter, r *http.Request) {
	request, err := decodeRiskTierRequest(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_risk_tier", "message": err.Error()})
		return
	}
	actor := store.ActorFromContext(r.Context())
	if actor.ID == "" {
		http.Error(w, "an authenticated person is required", http.StatusUnauthorized)
		return
	}
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return
	}
	if workspace, scoped := store.WorkspaceFromContext(r.Context()); scoped && task.Workspace != workspace {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	_, err = s.Store.RaiseTaskRiskTier(r.Context(), core.RiskRating{
		TaskID: task.ID, Tier: riskpolicy.Tier(request.Tier), Reason: request.Reason, ActorID: actor.ID, HeadSHA: task.ReviewedHeadSHA,
	})
	var notRaised *core.RiskTierNotRaisedError
	switch {
	case errors.As(err, &notRaised):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "risk_tier_not_raised", "message": notRaised.Error(), "current_tier": string(notRaised.Current)})
		return
	case errors.Is(err, store.ErrRiskNotEnabled):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "risk_not_enabled", "message": err.Error()})
		return
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "task not found", http.StatusNotFound)
		return
	case err != nil:
		writeStoreFailure(w, r, err)
		return
	}
	current, err := s.Store.GetTask(r.Context(), task.ID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	risk, err := s.taskRisk(r.Context(), current)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": task.ID, "risk": risk})
}
