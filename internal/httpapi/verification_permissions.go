package httpapi

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// VK-4 / AC-7.3: only authenticated workspace operators can grant authority.
// This endpoint is deliberately absent from MCP and worker registrations.
func (s *Server) grantVerificationPermissions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("workspace_id") == "" {
		verificationReadError(w, store.ErrVerificationInvalid)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser || credential.OwnerUserID == "" {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	var input store.VerificationPermissionRequest
	data, err := io.ReadAll(io.LimitReader(r.Body, (256<<10)+1))
	if err != nil || len(data) > 256<<10 || core.DecodeVerificationRequest(data, &input) != nil {
		verificationReadError(w, store.ErrVerificationInvalid)
		return
	}
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	b, ok := s.Store.(store.VerificationStore)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	kind := store.VerificationGrantPermissions
	if input.RevokeGrantID != "" {
		kind = store.VerificationRevokePermissions
	}
	receipt, err := b.ApplyVerification(r.Context(), store.VerificationCommand{Access: store.VerificationAccess{UserID: credential.OwnerUserID, TaskID: order.TaskID, WorkOrderID: order.ID}, Kind: kind, ContextID: input.ContextID, Key: input.RequestKey, Permissions: &input})
	if err != nil {
		verificationReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
