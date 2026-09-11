package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Server) restartTask(w http.ResponseWriter, r *http.Request) {
	var request core.TaskStartOverRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid restart request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "restart body must contain one JSON object", http.StatusBadRequest)
		return
	}
	request.TaskID = chi.URLParam(r, "id")
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" {
		request.RequestID = strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if err := request.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	if s.Memberships != nil {
		var err error
		if ws, ok := store.WorkspaceFromContext(r.Context()); ok && s.Workspaces != nil {
			request.CanConfirmDocuments, err = s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, ws, core.CapabilityConfirmDocuments)
		} else {
			request.CanConfirmDocuments, err = s.Memberships.AuthorizeDeployment(r.Context(), credential.OwnerUserID, core.CapabilityConfirmDocuments)
		}
		if err != nil {
			log.Printf("authorize task start over: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	} else {
		request.CanConfirmDocuments = credential.Scope == core.CredentialScopeOperator
	}
	result, err := taskops.New(s.Store).StartOver(r.Context(), request)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTaskTerminal):
			http.Error(w, "terminal tasks cannot start over; create a task with POST /v1/tasks", http.StatusConflict)
		case errors.Is(err, store.ErrStartOverRequestConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrStartOverConfirmDocuments):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "task not found", http.StatusNotFound)
		default:
			log.Printf("start task over: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	if result.Created && s.OnCreate != nil {
		s.OnCreate(r.Context(), result.Successor.ID)
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
