package httpapi

import (
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// WorkspaceInfo is the read-only workspace snapshot served at
// GET /v1/workspace: identity, repos, stage routing, and credential
// metadata. It is derived from the current database-backed workspace document
// while deployment and credential metadata remain file-backed (spec §21.3).
// Credential refs are excluded outright: refs are metadata rather than
// secrets (spec §5.2), but they name host paths and env vars, and the read API
// is unauthenticated.
type WorkspaceInfo struct {
	Workspace   string                `json:"workspace"`
	Image       string                `json:"image"`
	MaxBounces  int                   `json:"max_bounces"`
	Database    string                `json:"database"`
	Repos       []WorkspaceRepo       `json:"repos"`
	Routing     []WorkspaceRoute      `json:"routing"`
	Credentials []WorkspaceCredential `json:"credentials"`
}

type WorkspaceRepo struct {
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	GitHub          string     `json:"github,omitempty"`
	Base            string     `json:"base"`
	Image           string     `json:"image"`
	SecretRefCount  int        `json:"secret_ref_count"`
	AllowedCommands [][]string `json:"allowed_commands,omitempty"`
	DeniedCommands  [][]string `json:"denied_commands,omitempty"`
}

type WorkspaceRoute struct {
	Stage     string   `json:"stage"`
	Harnesses []string `json:"harnesses"`
	ModelTier string   `json:"model_tier,omitempty"`
	BudgetUSD float64  `json:"budget_usd"`
	Timeout   string   `json:"timeout"`
}

type WorkspaceCredential struct {
	ID        string `json:"id"`
	OwnerID   string `json:"owner_id"`
	OwnerKind string `json:"owner_kind"`
	Kind      string `json:"kind"`
	Vendor    string `json:"vendor"`
	Harness   string `json:"harness"`
}

// Stage order for the routing table; map iteration order would jitter the UI.
var routeOrder = []string{"triage", "spec", "implement", "review", "verify", "gate", "merge", "monitor"}

func NewWorkspaceInfo(cfg *config.Config) *WorkspaceInfo {
	info := &WorkspaceInfo{
		Workspace:  cfg.Workspace,
		Image:      cfg.Image,
		MaxBounces: cfg.MaxBounces,
		Database:   cfg.Database.Backend,
	}
	for _, repo := range cfg.Repos {
		info.Repos = append(info.Repos, WorkspaceRepo{
			Name:            repo.Name,
			URL:             repo.URL,
			GitHub:          repo.GitHub,
			Base:            repo.Base,
			Image:           repo.Image,
			SecretRefCount:  len(repo.SecretRefs),
			AllowedCommands: repo.ToolPolicy.AllowedCommands,
			DeniedCommands:  repo.ToolPolicy.DeniedCommands,
		})
	}
	seen := make(map[string]bool, len(cfg.Routing.Stages))
	appendRoute := func(stage string, route config.StageRoute) {
		timeout := route.Timeout
		if timeout == 0 {
			timeout = config.DefaultStageTimeout
		}
		info.Routing = append(info.Routing, WorkspaceRoute{
			Stage:     stage,
			Harnesses: route.Harnesses,
			ModelTier: route.ModelTier,
			BudgetUSD: route.BudgetUSD,
			Timeout:   timeout.String(),
		})
		seen[stage] = true
	}
	for _, stage := range routeOrder {
		if route, ok := cfg.Routing.Stages[stage]; ok {
			appendRoute(stage, route)
		}
	}
	for stage, route := range cfg.Routing.Stages {
		if !seen[stage] {
			appendRoute(stage, route)
		}
	}
	for _, credential := range cfg.Credentials {
		info.Credentials = append(info.Credentials, WorkspaceCredential{
			ID:        credential.ID,
			OwnerID:   credential.OwnerID,
			OwnerKind: credential.OwnerKind,
			Kind:      credential.Kind,
			Vendor:    credential.Vendor,
			Harness:   credential.Harness,
		})
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
