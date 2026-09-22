package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// KitPermissionGrant is client-local. It never enters WorkspaceDocument or
// the frozen pipeline policy (VK-4; req-execution-configuration REQ-3).
type KitPermissionGrant struct {
	Server     string                                `yaml:"server" json:"server"`
	Workspace  string                                `yaml:"workspace" json:"workspace"`
	Repository string                                `yaml:"repository" json:"repository"`
	Binding    string                                `yaml:"binding" json:"binding"`
	Actions    []verification.VerificationPermission `yaml:"actions" json:"actions"`
}

func (g KitPermissionGrant) Validate() error {
	u, err := url.Parse(g.Server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("kit_permissions: explicit server URL required")
	}
	if g.Workspace == "" || g.Repository == "" || !verification.VerificationBindingName(g.Binding) {
		return fmt.Errorf("kit_permissions: workspace, repository and binding required")
	}
	if _, err := verification.NormalizeVerificationPermissions(g.Actions); err != nil {
		return err
	}
	for _, a := range g.Actions {
		if a.Binding != g.Binding {
			return fmt.Errorf("kit_permissions: action binding differs from grant binding")
		}
	}
	return nil
}

func (c *Config) KitActions(server, workspace, repository string) ([]verification.VerificationPermission, error) {
	out := []verification.VerificationPermission{}
	found := false
	for _, g := range c.KitPermissions {
		if err := g.Validate(); err != nil {
			return nil, err
		}
		if strings.TrimRight(g.Server, "/") == strings.TrimRight(server, "/") && g.Workspace == workspace && g.Repository == repository {
			found = true
			out = append(out, g.Actions...)
		}
	}
	if !found {
		return nil, fmt.Errorf("missing local kit_permissions grant for server %s workspace %s repository %s", server, workspace, repository)
	}
	return verification.NormalizeVerificationPermissions(out)
}
