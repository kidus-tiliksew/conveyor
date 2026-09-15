package httpapi

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"net/http"
)

func (s *Server) workerWorktreeHandoff(w http.ResponseWriter, r *http.Request) {
	worker, ok := workerFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	var request core.WorktreeHandoffRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6<<20)).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.respondWorktreeHandoff(w, r, chi.URLParam(r, "id"), core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: request.SessionID}, request)
}
func (s *Server) runWorktreeHandoff(w http.ResponseWriter, r *http.Request) {
	var request core.WorktreeHandoffRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6<<20)).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "order_id"))
	if !ok || err != nil || order.TaskID != chi.URLParam(r, "id") {
		http.Error(w, "claim identity unavailable", 409)
		return
	}
	workspace, _ := store.WorkspaceFromContext(r.Context())
	// Historical release is validated in the serialized command; a bound child
	// remains confined to its original workspace, order, session, and user.
	if credential.Kind != core.CredentialUser && !(boundRunChildCredential(credential) && credential.RunWorkspaceID == workspace && credential.RunWorkOrderID == order.ID && credential.RunSessionID == request.SessionID) {
		http.Error(w, "claim identity unavailable", 409)
		return
	}
	s.respondWorktreeHandoff(w, r, order.ID, core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID(credential.OwnerUserID), SessionID: request.SessionID}, request)
}
func (s *Server) respondWorktreeHandoff(w http.ResponseWriter, r *http.Request, id string, claim core.WorkOrderClaimIdentity, request core.WorktreeHandoffRequest) {
	if s.Workers == nil {
		http.Error(w, "worktree service unavailable", 503)
		return
	}
	result, err := s.Workers.WorktreeHandoff(r.Context(), id, claim, request)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSON(w, 200, result)
}
