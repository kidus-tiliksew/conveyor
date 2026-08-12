package httpapi

import (
	"encoding/json"
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.NotFound(w, r)
		return
	}
	if task.Assignee != nil && task.Assignee.UserID != credential.OwnerUserID {
		operator := credential.Scope == core.CredentialScopeOperator
		if s.Memberships != nil {
			workspaceID, _ := store.WorkspaceFromContext(r.Context())
			operator, err = s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, workspaceID, core.CapabilitySetAssignee)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found || latest.Stage != core.StageReview {
		http.Error(w, "task is not at the merge gate", http.StatusConflict)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	maxBounces := 1
	if s.ConfigProvider != nil {
		cfg, cfgErr := s.ConfigProvider(r.Context())
		if cfgErr != nil {
			http.Error(w, cfgErr.Error(), http.StatusInternalServerError)
			return
		}
		maxBounces = cfg.MaxBounces
	} else if s.Deployment != nil {
		maxBounces = s.Deployment.MaxBounces
	}
	updated, err := taskops.New(s.Store).RequestChanges(r.Context(), taskops.RequestChanges{
		TaskID: taskID, JobID: latest.ID, Feedback: request.Feedback,
		MaxBounces: maxBounces, Hold: store.UserRequestChangesHold(events),
	})
	if err != nil {
		status := http.StatusInternalServerError
		var invalid *core.ErrInvalidTransition
		if errors.As(err, &invalid) || strings.Contains(err.Error(), "not at the merge gate") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if !s.Store.IsDurable() && s.OnCreate != nil {
		s.OnCreate(r.Context(), taskID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": updated, "feedback": request.Feedback})
}
