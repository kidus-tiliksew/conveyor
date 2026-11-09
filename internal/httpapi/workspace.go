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
	Workspace  string                `json:"workspace"`
	MaxBounces int                   `json:"max_bounces"`
	Recovery   config.RecoveryPolicy `json:"recovery"`
	Database   string                `json:"database"`
	Repos      []config.Repo         `json:"repos"`
	// DefaultPlanState is the effective execution.default_plan_state, needed
	// when unset (component-runtime invariant 39).
	DefaultPlanState string `json:"default_plan_state"`
}

func NewWorkspaceInfo(cfg *config.Config) *WorkspaceInfo {
	policy := cfg.Recovery
	if !policy.Present() {
		policy = config.DefaultRecoveryPolicy()
	}
	execution := cfg.Execution
	if err := config.NormalizeDefaultPlanState(&execution); err != nil {
		execution.DefaultPlanState = config.DefaultPlanStateNeeded
	}
	return &WorkspaceInfo{Workspace: cfg.Workspace, MaxBounces: cfg.MaxBounces, Recovery: policy, Database: cfg.Database.Backend, Repos: append([]config.Repo(nil), cfg.Repos...), DefaultPlanState: execution.DefaultPlanState}
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
