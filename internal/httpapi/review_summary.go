package httpapi

// Review round summary surfaces: MCP submit_review_summary and REST
// POST /v1/tasks/{id}/review-rounds/{round}/summaries
// (req-review-gates-evidence AC-10.3, AC-10.4; DEC-19; DEC-79;
// feature-review-round-summary SU-4). Both reach the same
// workorder.Service.RecordReviewSummary admission and store command.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const reviewSummaryTool = "submit_review_summary"

// reviewSummaryMCPTool is the published schema of submit_review_summary.
func reviewSummaryMCPTool() map[string]any {
	str := map[string]string{"type": "string"}
	finding := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"severity": map[string]any{"type": "string", "enum": []string{store.ReviewSummarySeverityBlocking, store.ReviewSummarySeverityNonBlocking}},
			"problem":  str,
			"location": str,
			"fix":      str,
			"seats":    map[string]any{"type": "array", "items": map[string]string{"type": "integer"}},
		},
		"required":             []string{"severity", "problem", "seats"},
		"additionalProperties": false,
	}
	return map[string]any{
		"name":        reviewSummaryTool,
		"description": "Record an informative summary of a settled review round that consolidates its seats' findings. It changes no verdict, bounce, gate, or work order. The round must have settled; every seat that requested changes must be cited by at least one finding. A user credential needs claim_work; an agent credential must pass the work_order_id and session_id of an order of the same task that its session holds claimed, submitted, or completed. Each accepted request becomes the round's next version; repeating a request_id with identical content returns the recorded version.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace_id": str, "task_id": str, "review_round": map[string]string{"type": "integer"},
				"request_id": str, "summary": str,
				"findings":     map[string]any{"type": "array", "items": finding, "maxItems": store.ReviewSummaryMaxFindings},
				"author_model": str, "work_order_id": str, "session_id": str,
			},
			"required":             []string{"task_id", "review_round", "request_id", "summary", "findings"},
			"additionalProperties": false,
		},
	}
}

// callReviewSummaryMCP admits and records one submit_review_summary call. It
// applies the generic MCP boundary itself (worker refusal, agent capability
// ceiling, workspace resolution, and live claim_work membership) so every
// admission refusal is the one ErrWorkOrderClaimUnauthorized the service
// returns, and never discloses whether the task exists (SU-3; DEC-19).
func (s *Server) callReviewSummaryMCP(r *http.Request, args map[string]any) (any, error) {
	ctx := r.Context()
	if _, workerAuth := workerFromContext(ctx); workerAuth {
		return nil, store.ErrWorkOrderClaimUnauthorized
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok {
		return nil, store.ErrWorkOrderClaimUnauthorized
	}
	capability := mcpCapability(reviewSummaryTool)
	if credential.Kind == core.CredentialAgent && !agentMayExerciseCapability(capability) {
		return nil, store.ErrWorkOrderClaimUnauthorized
	}
	explicit, _ := args["workspace_id"].(string)
	workspace, err := s.resolveMCPWorkspace(ctx, explicit)
	if errors.Is(err, errWorkspaceNotVisible) {
		return nil, store.ErrWorkOrderClaimUnauthorized
	}
	if err != nil {
		return nil, err
	}
	ctx = store.WithWorkspace(ctx, workspace)
	if s.Workspaces != nil {
		if s.Memberships == nil {
			return nil, store.ErrWorkOrderClaimUnauthorized
		}
		allowed, authErr := s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspace, capability)
		if authErr != nil {
			return nil, authErr
		}
		if !allowed {
			return nil, store.ErrWorkOrderClaimUnauthorized
		}
	}
	if s.WorkOrders == nil {
		return nil, fmt.Errorf("work-order service unavailable")
	}
	input, err := reviewSummaryMCPInput(args)
	if err != nil {
		return nil, err
	}
	result, err := s.WorkOrders.RecordReviewSummary(ctx, input)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// reviewSummaryMCPInput reads the typed tool arguments. A mistyped argument is
// refused rather than read as empty.
func reviewSummaryMCPInput(args map[string]any) (workorder.ReviewSummaryInput, error) {
	var input workorder.ReviewSummaryInput
	text := func(key string, target *string) error {
		value, present := args[key]
		if !present || value == nil {
			return nil
		}
		typed, ok := value.(string)
		if !ok {
			return invalidToolArgument(fmt.Errorf("invalid_argument: %s must be a string", key))
		}
		*target = typed
		return nil
	}
	for key, target := range map[string]*string{
		"task_id": &input.TaskID, "request_id": &input.RequestID, "summary": &input.Summary,
		"author_model": &input.AuthorModel, "work_order_id": &input.WorkOrderID, "session_id": &input.SessionID,
	} {
		if err := text(key, target); err != nil {
			return workorder.ReviewSummaryInput{}, err
		}
	}
	round, ok := args["review_round"].(float64)
	if !ok || round != math.Trunc(round) || round < math.MinInt32 || round > math.MaxInt32 {
		return workorder.ReviewSummaryInput{}, invalidToolArgument(fmt.Errorf("invalid_argument: review_round must be an integer"))
	}
	input.ReviewRound = int(round)
	if raw, present := args["findings"]; present && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return workorder.ReviewSummaryInput{}, invalidToolArgument(fmt.Errorf("invalid_argument: findings: %w", err))
		}
		if input.Findings, err = decodeReviewSummaryFindings(encoded); err != nil {
			return workorder.ReviewSummaryInput{}, err
		}
	}
	return input, nil
}

func decodeReviewSummaryFindings(raw []byte) ([]store.ReviewSummaryFinding, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var findings []store.ReviewSummaryFinding
	if err := decoder.Decode(&findings); err != nil {
		return nil, invalidToolArgument(fmt.Errorf("invalid_argument: findings must be a list of {severity, problem, location, fix, seats}: %w", err))
	}
	return findings, nil
}

// reviewSummaryRequestBody is the REST body: the MCP arguments without
// task_id and review_round, which come from the path. The route admits only
// user credentials, whose author record carries no order or session, and the
// workspace comes from the request's resolved workspace context.
type reviewSummaryRequestBody struct {
	RequestID   string          `json:"request_id"`
	Summary     string          `json:"summary"`
	Findings    json.RawMessage `json:"findings"`
	AuthorModel string          `json:"author_model"`
}

// recordReviewSummary serves POST /v1/tasks/{id}/review-rounds/{round}/summaries
// for user credentials. The route's claim_work capability boundary answers the
// canonical 404 before any lookup; the service's uniform refusal answers 409.
// A new version answers 201 and an identical replay 200.
func (s *Server) recordReviewSummary(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	round, err := strconv.Atoi(chi.URLParam(r, "round"))
	if err != nil {
		http.Error(w, "invalid_argument: review round must be an integer", http.StatusBadRequest)
		return
	}
	var body reviewSummaryRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		http.Error(w, "invalid_argument: "+err.Error(), http.StatusBadRequest)
		return
	}
	input := workorder.ReviewSummaryInput{
		TaskID: chi.URLParam(r, "id"), ReviewRound: round, RequestID: body.RequestID, Summary: body.Summary,
		AuthorModel: body.AuthorModel,
	}
	if len(body.Findings) > 0 && !bytes.Equal(bytes.TrimSpace(body.Findings), []byte("null")) {
		if input.Findings, err = decodeReviewSummaryFindings(body.Findings); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	result, err := s.WorkOrders.RecordReviewSummary(r.Context(), input)
	switch {
	case err == nil && result.Replayed:
		writeJSON(w, http.StatusOK, result)
	case err == nil:
		writeJSON(w, http.StatusCreated, result)
	case errors.Is(err, store.ErrReviewSummaryInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrReviewSummaryRoundNotSettled), errors.Is(err, store.ErrReviewSummaryConflict), errors.Is(err, store.ErrWorkOrderClaimUnauthorized):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		log.Printf("record review summary: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
