package httpapi

import (
	"net/http"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

type WorkspaceInfo struct {
	Workspace  string           `json:"workspace"`
	MaxBounces int              `json:"max_bounces"`
	Database   string           `json:"database"`
	Repos      []config.Repo    `json:"repos"`
	Routing    []WorkspaceRoute `json:"routing"`
}
type WorkspaceRoute struct {
	Stage     string               `json:"stage"`
	Model     string               `json:"model"`
	BudgetUSD float64              `json:"budget_usd"`
	Timeout   string               `json:"timeout"`
	Execution config.ExecutionMode `json:"execution"`
}

func NewWorkspaceInfo(cfg *config.Config) *WorkspaceInfo {
	info := &WorkspaceInfo{Workspace: cfg.Workspace, MaxBounces: cfg.MaxBounces, Database: cfg.Database.Backend, Repos: append([]config.Repo(nil), cfg.Repos...)}
	stages := make([]string, 0, len(cfg.Routing.Stages))
	for stage := range cfg.Routing.Stages {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	for _, stage := range stages {
		route := cfg.Routing.Stages[stage]
		info.Routing = append(info.Routing, WorkspaceRoute{Stage: stage, Model: route.Model, BudgetUSD: route.BudgetUSD, Timeout: route.TimeoutText, Execution: route.Execution})
	}
	return info
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
