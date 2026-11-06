package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Server) requestTaskChanges(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Feedback string `json:"feedback"`
	}
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	if strings.TrimSpace(request.Feedback) == "" {
		http.Error(w, "feedback is required", http.StatusBadRequest)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	taskID := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeStoreFailure(w, r, err)
		return
	}
	if task.Assignee != nil && task.Assignee.UserID != credential.OwnerUserID {
		operator := credential.Scope == core.CredentialScopeOperator
		if s.Memberships != nil {
			workspaceID, _ := store.WorkspaceFromContext(r.Context())
			operator, err = s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, workspaceID, core.CapabilitySetAssignee)
			if err != nil {
				writeStoreFailure(w, r, err)
				return
			}
		}
		if !operator {
			http.NotFound(w, r)
			return
		}
	}
	latest, found, err := s.Store.GetLatestJob(r.Context(), taskID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if !found || latest.Stage != core.StageReview {
		http.Error(w, store.NotAtMergeGateError(task).Error(), http.StatusConflict)
		return
	}
	maxBounces := 1
	if task.SetupContract.MaxBounces > 0 {
		maxBounces = task.SetupContract.MaxBounces
	} else if s.ConfigProvider != nil {
		cfg, cfgErr := s.ConfigProvider(r.Context())
		if cfgErr != nil {
			writeStoreFailure(w, r, cfgErr)
			return
		}
		maxBounces = cfg.MaxBounces
	} else if s.Deployment != nil {
		maxBounces = s.Deployment.MaxBounces
	}
	updated, err := taskops.New(s.Store).RequestChanges(r.Context(), taskops.RequestChanges{
		TaskID: taskID, JobID: latest.ID, Feedback: request.Feedback,
		MaxBounces: maxBounces,
	})
	if err != nil {
		var invalid *core.ErrInvalidTransition
		if errors.As(err, &invalid) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeStoreFailure(w, r, err)
		return
	}
	if !s.Store.IsDurable() && s.OnCreate != nil {
		s.OnCreate(r.Context(), taskID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": updated, "feedback": request.Feedback})
}
