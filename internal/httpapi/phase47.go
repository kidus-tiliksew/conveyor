package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const maxArtifactBytes = 25 << 20

func (s *Server) listWorkOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Store.ListWorkOrders(r.Context())
	if err != nil {
		log.Printf("handle work order request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if orders == nil {
		orders = []core.WorkOrder{}
	}
	orders = projectAssigneeClaimability(r.Context(), orders, s.ForgeTokens)
	orders, err = s.projectWorkOrderSessions(r.Context(), orders, false, core.Worker{})
	if err != nil {
		log.Printf("project work order sessions: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, 200, orders)
}

// projectWorkOrderSessions removes bearer session material unless the caller
// is the principal that owns the claim. This projection is for list surfaces;
// claim-time continuation and exact claimed-order reads keep their contracts.
func (s *Server) projectWorkOrderSessions(ctx context.Context, orders []core.WorkOrder, workerAuth bool, worker core.Worker) ([]core.WorkOrder, error) {
	credential, credentialAuth := store.CredentialFromContext(ctx)
	ownedWorkers := map[string]bool{}
	if !workerAuth && credentialAuth && credential.Kind == core.CredentialAgent {
		workers, err := s.Store.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		for _, candidate := range workers {
			if candidate.OwnerUserID == credential.OwnerUserID {
				ownedWorkers[candidate.ID] = true
			}
		}
	}
	for i := range orders {
		owner := false
		switch {
		case workerAuth:
			owner = orders[i].WorkerID != "" && orders[i].WorkerID == worker.ID
		case credentialAuth && credential.Kind == core.CredentialUser:
			owner = orders[i].WorkerID == "" && orders[i].ClaimantID == core.TaskRunClaimantID(credential.OwnerUserID)
		case credentialAuth && credential.Kind == core.CredentialAgent:
			owner = (orders[i].WorkerID == "" && orders[i].ClaimantID == core.TaskRunClaimantID(credential.OwnerUserID)) ||
				(orders[i].WorkerID != "" && ownedWorkers[orders[i].WorkerID])
		}
		if !owner {
			orders[i].SessionID = ""
		}
	}
	return orders, nil
}

func projectAssigneeClaimability(ctx context.Context, orders []core.WorkOrder, tokenSources ...store.ForgeTokenStore) []core.WorkOrder {
	credential, authenticated := store.CredentialFromContext(ctx)
	var tokenErr error
	if authenticated && len(tokenSources) > 0 && tokenSources[0] != nil {
		tokenErr = store.RequireForgeTokenPresence(ctx, tokenSources[0], credential.OwnerUserID)
	}
	for i := range orders {
		if orders[i].Assignee != nil && (!authenticated || orders[i].Assignee.UserID != credential.OwnerUserID) {
			orders[i].Claimable = false
			orders[i].ClaimRefusalReason = fmt.Sprintf("task is assigned to %s; only that assignee may claim its work orders", orders[i].Assignee.UserID)
		} else if tokenErr != nil && orders[i].State == core.WorkOrderQueued && orders[i].Claimable {
			orders[i].Claimable = false
			orders[i].ClaimRefusalReason = store.ForgeTokenRequiredMessage
		}
	}
	return orders
}

func (s *Server) recoverWorkOrder(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.RequestID == "" {
		request.RequestID = r.Header.Get("X-Idempotency-Key")
	}
	if _, err := core.NormalizeWorkOrderOperatorDirection(request.Direction); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	order, err := s.WorkOrders.Recover(r.Context(), chi.URLParam(r, "id"), request.RequestID, request.Direction)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) retryReviewRound(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.RequestID == "" {
		request.RequestID = r.Header.Get("X-Idempotency-Key")
	}
	if strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.Reason) == "" {
		http.Error(w, "review retry request_id and operator reason are required", http.StatusBadRequest)
		return
	}
	result, err := s.WorkOrders.RetryReviewRound(r.Context(), chi.URLParam(r, "id"), request.RequestID, request.Reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) recoverInterruptedReviewRound(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.RequestID == "" {
		request.RequestID = r.Header.Get("X-Idempotency-Key")
	}
	if strings.TrimSpace(request.RequestID) == "" {
		http.Error(w, "interrupted review recovery request_id is required", http.StatusBadRequest)
		return
	}
	result, err := s.WorkOrders.RecoverInterruptedReviewRound(r.Context(), chi.URLParam(r, "id"), request.RequestID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getLifecycleDiagram(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"mermaid": core.LifecycleStateDiagram()})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	artifacts, err := s.Store.ListArtifacts(r.Context())
	if err != nil {
		log.Printf("handle work order request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for i := range artifacts {
		artifacts[i].DownloadURL = "/v1/artifacts/" + artifacts[i].ID
	}
	writeJSON(w, 200, artifacts)
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBytes+(1<<20))
	if err := r.ParseMultipartForm(maxArtifactBytes); err != nil {
		http.Error(w, "artifact exceeds 25 MiB or form is invalid", http.StatusRequestEntityTooLarge)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", 400)
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(content) > maxArtifactBytes {
		http.Error(w, "artifact exceeds 25 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	sum := sha256.Sum256(content)
	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(content)
	}
	workspace := s.Workspace
	if s.ConfigProvider != nil {
		if cfg, getErr := s.ConfigProvider(r.Context()); getErr == nil {
			workspace = cfg.Workspace
		}
	}
	role := core.ArtifactRole(strings.TrimSpace(r.FormValue("role")))
	if role == "" {
		role = core.ArtifactRoleTaskContext
	}
	if role != core.ArtifactRoleTaskContext && role != core.ArtifactRoleVerificationEvidence {
		http.Error(w, "artifact role must be task_context or verification_evidence", http.StatusBadRequest)
		return
	}
	artifact := core.Artifact{
		ID: fmt.Sprintf("%x", sum), Workspace: workspace, Name: safeFilename(header),
		ContentType: contentType, SizeBytes: int64(len(content)), Role: role,
		TaskID: strings.TrimSpace(r.FormValue("task_id")),
		// Requirement attachments replace feature attachments on the live API;
		// the store retains legacy fields only for migration compatibility
		// by construction.
		RequirementID:     strings.TrimSpace(r.FormValue("requirement_id")),
		PlanningSessionID: strings.TrimSpace(r.FormValue("planning_session_id")),
		CreatedAt:         time.Now().UTC(),
	}
	artifact, err = s.Store.CreateArtifact(r.Context(), artifact, content)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 201, artifact)
}

func safeFilename(header *multipart.FileHeader) string {
	name := strings.TrimSpace(header.Filename)
	if name == "" {
		return "artifact"
	}
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	artifact, content, err := s.Store.GetArtifact(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.Header().Set("Content-Type", artifact.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", artifact.Name))
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	_, _ = w.Write(content)
}
