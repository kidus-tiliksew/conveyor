package httpapi

import (
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// WorkspaceInfo is the workspace summary GET /v1/workspace serves. It is a
// policy read: no stage route, model, effort, setup, or composed
// control-plane setting leaves the daemon through it (component-http-api;
// component-runtime; DEC-56).
type WorkspaceInfo struct {
	Workspace  string        `json:"workspace"`
	MaxBounces int           `json:"max_bounces"`
	Database   string        `json:"database"`
	Repos      []config.Repo `json:"repos"`
}

func NewWorkspaceInfo(cfg *config.Config) *WorkspaceInfo {
	return &WorkspaceInfo{Workspace: cfg.Workspace, MaxBounces: cfg.MaxBounces, Database: cfg.Database.Backend, Repos: append([]config.Repo(nil), cfg.Repos...)}
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.ConfigProvider != nil {
		cfg, err := s.ConfigProvider(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, NewWorkspaceInfo(cfg))
		return
	}
	if s.WorkspaceInfo == nil {
		http.Error(w, "workspace config unavailable", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.WorkspaceInfo)
}
