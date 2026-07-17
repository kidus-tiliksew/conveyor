package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

var workspaceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type createWorkspaceRequest struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Document json.RawMessage `json:"document,omitempty"`
}

type createWorkspaceDocument struct {
	Workspace                 *string                 `json:"workspace,omitempty"`
	MaxBounces                *int                    `json:"max_bounces,omitempty"`
	WorkOrderQueueTimeoutText *string                 `json:"work_order_queue_timeout,omitempty"`
	Routing                   *config.Routing         `json:"routing,omitempty"`
	Repos                     *[]config.Repo          `json:"repos,omitempty"`
	Harnesses                 *[]config.Harness       `json:"harnesses,omitempty"`
	Review                    *config.ReviewPanel     `json:"review,omitempty"`
	Execution                 *config.ExecutionPolicy `json:"execution,omitempty"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if s.Workspaces == nil {
		items := []core.Workspace{}
		if s.Workspace != "" {
			items = append(items, core.Workspace{ID: s.Workspace, Name: s.Workspace, ConfigVersion: 1})
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	items, err := s.Workspaces.ListWorkspaces(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getWorkspaceRecord(w http.ResponseWriter, r *http.Request) {
	id, _ := store.WorkspaceFromContext(r.Context())
	if s.Workspaces == nil {
		writeJSON(w, http.StatusOK, core.Workspace{ID: id, Name: id, ConfigVersion: 1})
		return
	}
	item, err := s.Workspaces.GetWorkspace(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.Workspaces == nil || s.Deployment == nil {
		http.Error(w, "workspace creation unavailable", http.StatusNotFound)
		return
	}
	var request createWorkspaceRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "workspace", err)
		return
	}
	request.ID, request.Name = strings.TrimSpace(request.ID), strings.TrimSpace(request.Name)
	if !workspaceIDPattern.MatchString(request.ID) {
		writeValidationError(w, "id", errors.New("id must be a lowercase slug of 1-63 letters, digits, or hyphens"))
		return
	}
	if request.Name == "" || len(request.Name) > 200 {
		writeValidationError(w, "name", errors.New("name is required and must be at most 200 characters"))
		return
	}
	base := *s.Deployment
	base.Workspace = request.ID
	var next *config.Config
	var err error
	if len(request.Document) == 0 || string(request.Document) == "null" {
		next = &base
	} else {
		var partial createWorkspaceDocument
		partialDecoder := json.NewDecoder(bytes.NewReader(request.Document))
		partialDecoder.DisallowUnknownFields()
		if decodeErr := partialDecoder.Decode(&partial); decodeErr != nil {
			writeValidationError(w, "document", decodeErr)
			return
		}
		document := base.WorkspaceDocument()
		document.Workspace = request.ID
		if partial.Workspace != nil && *partial.Workspace != "" && *partial.Workspace != request.ID {
			writeValidationError(w, "document.workspace", errors.New("document workspace must match id"))
			return
		}
		if partial.MaxBounces != nil {
			document.MaxBounces = *partial.MaxBounces
		}
		if partial.WorkOrderQueueTimeoutText != nil {
			document.WorkOrderQueueTimeoutText = *partial.WorkOrderQueueTimeoutText
		}
		if partial.Routing != nil {
			document.Routing = *partial.Routing
		}
		if partial.Repos != nil {
			document.Repos = *partial.Repos
		}
		if partial.Harnesses != nil {
			document.Harnesses = *partial.Harnesses
		}
		if partial.Review != nil {
			if len(partial.Review.Seats) == 0 {
				writeValidationError(w, "review", errors.New("review.seats must contain at least one seat"))
				return
			}
			document.Review = *partial.Review
		}
		if partial.Execution != nil {
			document.Execution = *partial.Execution
		}
		data, marshalErr := yaml.Marshal(document)
		if marshalErr != nil {
			writeValidationError(w, "document", marshalErr)
			return
		}
		next, err = config.ParseWorkspaceDocument(data, &base, "workspace creation")
		if err != nil {
			writeValidationError(w, validationField(err), err)
			return
		}
	}
	if s.EnsureWorkspaceQueues != nil {
		if err := s.EnsureWorkspaceQueues(request.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	created, err := s.Workspaces.CreateWorkspace(r.Context(), request.ID, request.Name, next)
	if errors.Is(err, store.ErrWorkspaceConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "workspace_conflict", "message": "workspace id or name already exists"})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) resolveWorkspaceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathID := strings.TrimSpace(chi.URLParam(r, "workspace_id"))
		queryID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
		headerID := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
		if queryID != "" && headerID != "" && queryID != headerID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_conflict", "message": "workspace query and header disagree"})
			return
		}
		if pathID != "" && ((queryID != "" && queryID != pathID) || (headerID != "" && headerID != pathID)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_conflict", "message": "path workspace conflicts with supplied context"})
			return
		}
		explicit := pathID
		if explicit == "" {
			explicit = queryID
			if explicit == "" {
				explicit = headerID
			}
		}
		var items []core.Workspace
		if s.Workspaces != nil {
			var err error
			items, err = s.Workspaces.ListWorkspaces(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			fallback := s.Workspace
			if fallback == "" && s.Deployment != nil {
				fallback = s.Deployment.Workspace
			}
			if fallback == "" {
				fallback = "test"
			}
			items = []core.Workspace{{ID: fallback, Name: fallback}}
		}
		if explicit == "" {
			switch len(items) {
			case 0:
				writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace_unavailable", "message": "create a workspace first"})
				return
			case 1:
				explicit = items[0].ID
			default:
				writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace_required", "message": "explicit workspace context is required"})
				return
			}
		}
		found := false
		for _, item := range items {
			if item.ID == explicit {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found", "message": "workspace not found"})
			return
		}
		next.ServeHTTP(w, r.WithContext(store.WithWorkspace(r.Context(), explicit)))
	})
}
