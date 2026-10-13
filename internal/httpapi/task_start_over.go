package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Server) restartTask(w http.ResponseWriter, r *http.Request) {
	var request core.TaskStartOverRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_restart_request", "message": "invalid restart request"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_restart_request", "message": "restart body must contain one JSON object"})
		return
	}
	request.TaskID = chi.URLParam(r, "id")
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" {
		request.RequestID = strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if err := request.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_restart_request", "message": err.Error()})
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
			writeRestartInternalError(w, r)
			return
		}
	} else {
		request.CanConfirmDocuments = credential.Scope == core.CredentialScopeOperator
	}
	result, err := taskops.New(s.Store).StartOver(r.Context(), request)
	if err != nil {
		writeRestartError(w, r, err)
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

func writeRestartError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		referenceErr        *store.TaskContextReferenceError
		requirementArchived *store.RequirementArchivedError
		designArchived      *store.SystemDesignArchivedError
		dependencyErr       *store.TaskDependencyError
		branchErr           *store.BranchInUseError
		invalidTransition   *core.ErrInvalidTransition
	)
	switch {
	case errors.Is(err, store.ErrTaskTerminal):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_terminal", "message": "terminal tasks cannot start over; create a task with POST /v1/tasks"})
	case errors.Is(err, store.ErrStartOverRequestConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "start_over_request_conflict", "message": err.Error()})
	case errors.Is(err, store.ErrStartOverConfirmDocuments):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "start_over_confirm_documents", "message": err.Error()})
	case errors.As(err, &dependencyErr):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_dependency", "message": dependencyErr.Error()})
	case errors.As(err, &invalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": invalidTransition.Error()})
	case errors.As(err, &requirementArchived):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "requirement_archived", "message": requirementArchived.Error()})
	case errors.As(err, &designArchived):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "system_design_archived", "message": designArchived.Error()})
	case errors.As(err, &referenceErr):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_context_reference", "message": referenceErr.Error()})
	case errors.As(err, &branchErr) || errors.Is(err, store.ErrTaskBranchConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_branch_conflict", "message": err.Error()})
	case errors.Is(err, store.ErrTaskAlreadyStartedOver):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_already_started_over", "message": "task already has a successor; read it with GET /v1/tasks/{id}"})
	case errors.Is(err, store.ErrRetryable):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "retryable", "message": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "task not found"})
	default:
		log.Printf("start task over: %v", err)
		writeRestartInternalError(w, r)
	}
}

func writeRestartInternalError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-Id", middleware.GetReqID(r.Context()))
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error", "message": "internal server error"})
}
