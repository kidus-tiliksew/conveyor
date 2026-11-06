package httpapi

// Claim-bound delivery routes that serve the executing session under the MCP
// credential classes (component-mcp-protocol, Claim-bound delivery endpoints).

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// submitWorkOrderReview shares the MCP admission and submission contract, so
// the REST route enforces the same claimant, workspace, and session scope.
func (s *Server) submitWorkOrderReview(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID   string          `json:"session_id"`
		HeadSHA     string          `json:"head_sha"`
		WorkspaceID string          `json:"workspace_id"`
		ReviewSeats json.RawMessage `json:"review_seats"`
	}
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	if request.WorkspaceID == "" {
		request.WorkspaceID = r.Header.Get("X-Workspace-ID")
	}
	args := map[string]any{"work_order_id": chi.URLParam(r, "id"), "session_id": request.SessionID, "head_sha": request.HeadSHA, "workspace_id": request.WorkspaceID}
	// A present review_seats passes unchanged to the shared submit_for_review
	// parser, which validates it after admission (feature-review-seat-minimum
	// RS-4).
	if request.ReviewSeats != nil {
		value, err := submitReviewSeatsValue(request.ReviewSeats)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		args["review_seats"] = value
	}
	result, err := s.callMCPTool(r, "submit_for_review", args)
	if err != nil {
		writeWorkOrderToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// submitReviewSeatsValue decodes a present REST review_seats value with
// UseNumber, so JSON null stays a present nil the shared parser refuses and
// numbers are never rounded through float64 before validation.
func submitReviewSeatsValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func (s *Server) getSubmissionTemplate(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	// Every visibility refusal below (no binding, no membership authority or
	// credential, no claim_work) answers the
	// canonical 404 a nonexistent workspace gets, never 403
	// (req-accounts-and-membership AC-4.2; DEC-19; component-http-api).
	workspace, err := s.resolveMCPWorkspace(r.Context(), r.URL.Query().Get("workspace_id"))
	if err != nil {
		writeWorkOrderToolError(w, r, err)
		return
	}
	ctx := store.WithWorkspace(r.Context(), workspace)
	session := r.URL.Query().Get("session_id")
	if s.Workspaces != nil {
		credential, ok := store.CredentialFromContext(ctx)
		if !ok || s.Memberships == nil {
			writeWorkspaceNotFound(w)
			return
		}
		allowed, authErr := s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspace, core.CapabilityClaimWork)
		if authErr != nil {
			// An authorization-store failure is not a refusal; it answers
			// the same opaque 500 as requireWorkspaceCapability.
			log.Printf("authorize submission template: %v", authErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !allowed {
			writeWorkspaceNotFound(w)
			return
		}
	}
	if _, err = s.authorizeClaimantSession(ctx, chi.URLParam(r, "id"), session); err != nil {
		writeWorkOrderToolError(w, r, err)
		return
	}
	result, err := s.WorkOrders.PullRequestTemplate(ctx, chi.URLParam(r, "id"), session)
	if err != nil {
		writeWorkOrderToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
