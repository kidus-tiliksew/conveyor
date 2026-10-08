package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// component-verification-runner; req-verification-kits AC-7.3: only authenticated workspace operators can grant authority.
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
		verificationPermissionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

// verificationPermissionError reports a stable reason only when the store
// produced one after authorizing the operator; every other refusal keeps the
// generic verification mapping (component-verification-runner).
func verificationPermissionError(w http.ResponseWriter, err error) {
	reason, message, recovery, ok := store.VerificationRefusalDetail(err)
	if !ok {
		verificationReadError(w, err)
		return
	}
	code := http.StatusConflict
	if errors.Is(err, store.ErrVerificationInvalid) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": message, "reason": reason, "recovery": recovery})
}

// getVerificationPermissions is the operator grant projection of
// component-verification-runner. It reads retained state only and
// writes no record or event.
func (s *Server) getVerificationPermissions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser || credential.OwnerUserID == "" {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	var req store.VerificationPermissionViewRequest
	query := r.URL.Query()
	if len(query["workspace_id"]) != 1 || query.Get("workspace_id") == "" {
		verificationReadError(w, store.ErrVerificationInvalid)
		return
	}
	for key, values := range query {
		if len(values) != 1 {
			verificationReadError(w, store.ErrVerificationInvalid)
			return
		}
		switch key {
		case "workspace_id":
		case "context_id":
			req.ContextID, req.Explicit = values[0], true
		case "grant_id":
			req.GrantID = values[0]
		case "cursor":
			req.Cursor = values[0]
		case "limit":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 || n > store.VerificationPageLimit {
				verificationReadError(w, store.ErrVerificationInvalid)
				return
			}
			req.Limit = n
		default:
			verificationReadError(w, store.ErrVerificationInvalid)
			return
		}
	}
	ws, scoped := store.WorkspaceFromContext(r.Context())
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "id"))
	if err != nil || !scoped || order.Stage != core.StageVerify {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	task, err := s.Store.GetTask(r.Context(), order.TaskID)
	if err != nil || task.Workspace != ws {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	reader, ok := s.Store.(store.VerificationReader)
	b, ok2 := s.Store.(store.VerificationStore)
	if !ok || !ok2 {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	access := store.VerificationAccess{TaskID: task.ID, UserID: credential.OwnerUserID}
	if req.ContextID == "" {
		req.ContextID, err = latestOrderVerificationContext(r, reader, access, order.ID)
		if err != nil {
			verificationReadError(w, err)
			return
		}
	}
	var snapshot *store.VerificationSnapshot
	if req.ContextID != "" {
		read, err := b.ReadVerification(r.Context(), access, req.ContextID)
		if err != nil {
			verificationReadError(w, err)
			return
		}
		snapshot = &read
	}
	view, err := store.BuildVerificationPermissionView(task, order, snapshot, req, time.Now().UTC())
	if err != nil {
		verificationReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// latestOrderVerificationContext pages newest-first context headers until it
// finds one owned by the order. An order without a context returns "".
func latestOrderVerificationContext(r *http.Request, reader store.VerificationReader, access store.VerificationAccess, orderID string) (string, error) {
	cursor := ""
	for range 20 {
		page, err := reader.ReadVerificationPage(r.Context(), access, store.VerificationPageRequest{Kind: "contexts", Limit: store.VerificationPageLimit, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for _, item := range page.Items {
			var m map[string]string
			if json.Unmarshal(item.Metadata, &m) == nil && m["work_order_id"] == orderID {
				return item.ID, nil
			}
		}
		if page.NextCursor == "" {
			return "", nil
		}
		cursor = page.NextCursor
	}
	return "", nil
}
