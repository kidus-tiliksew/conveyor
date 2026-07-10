// Package config loads the deployment configuration for Phase 1: one
// workspace, its repos, and host paths. The full layered scope model
// (platform → workspace → task, spec §2.1) arrives with the pack
// machinery; this file is deliberately just enough to run the loop.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/secrets"
	"gopkg.in/yaml.v3"
)

type Repo struct {
	Name       string             `yaml:"name"`
	URL        string             `yaml:"url"`
	GitHub     string             `yaml:"github"` // owner/repo slug for gh; empty disables PR/issue flow
	Base       string             `yaml:"base"`   // default base branch
	SecretRefs []string           `yaml:"secret_refs"`
	ToolPolicy adapter.ToolPolicy `yaml:"tool_policy"`
}

type SecretSet struct {
	LocalEligible bool `yaml:"local_eligible"`
}

type Secrets struct {
	Root       string               `yaml:"root"`
	Backend    string               `yaml:"backend"` // sops (default) | plain
	SOPSBinary string               `yaml:"sops_binary"`
	SOPSConfig string               `yaml:"sops_config"`
	Sets       map[string]SecretSet `yaml:"sets"`
}

type Config struct {
	Workspace string `yaml:"workspace"`
	Image     string `yaml:"image"`
	CacheDir  string `yaml:"cache_dir"`
	JobsDir   string `yaml:"jobs_dir"`
	// CodexCredentials is the host directory with the user's Codex
	// login, mounted read-only into sandboxes (spec §5.2).
	CodexCredentials string  `yaml:"codex_credentials"`
	Secrets          Secrets `yaml:"secrets"`
	Repos            []Repo  `yaml:"repos"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Workspace == "" {
		c.Workspace = "default"
	}
	if c.Image == "" {
		c.Image = "conveyor-base:dev"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c.CacheDir = expandDefault(c.CacheDir, home, filepath.Join(home, ".conveyor", "cache"))
	c.JobsDir = expandDefault(c.JobsDir, home, filepath.Join(home, ".conveyor", "jobs"))
	c.CodexCredentials = expandDefault(c.CodexCredentials, home, filepath.Join(home, ".codex"))
	c.Secrets.Root = expandDefault(c.Secrets.Root, home, filepath.Join(home, ".conveyor", "secrets"))
	if c.Secrets.SOPSConfig != "" {
		c.Secrets.SOPSConfig = expandDefault(c.Secrets.SOPSConfig, home, "")
	}
	if c.Secrets.Backend == "" {
		c.Secrets.Backend = secrets.BackendSOPS
	}
	if c.Secrets.Backend != secrets.BackendSOPS && c.Secrets.Backend != secrets.BackendPlain {
		return nil, fmt.Errorf("secrets.backend must be %q or %q", secrets.BackendSOPS, secrets.BackendPlain)
	}
	if c.Secrets.SOPSBinary == "" {
		c.Secrets.SOPSBinary = "sops"
	}
	for i := range c.Repos {
		if c.Repos[i].Name == "" || c.Repos[i].URL == "" {
			return nil, fmt.Errorf("repo %d: name and url are required", i)
		}
		if !safePathSegment(c.Repos[i].Name) {
			return nil, fmt.Errorf("repo %d: name %q must be one path-safe segment", i, c.Repos[i].Name)
		}
		if c.Repos[i].Base == "" {
			c.Repos[i].Base = "main"
		}
		for _, rawRef := range c.Repos[i].SecretRefs {
			ref, err := secrets.ParseRef(rawRef)
			if err != nil {
				return nil, fmt.Errorf("repo %s: %w", c.Repos[i].Name, err)
			}
			if ref.Workspace != c.Workspace {
				return nil, fmt.Errorf("repo %s: secret ref workspace %q does not match configured workspace %q", c.Repos[i].Name, ref.Workspace, c.Workspace)
			}
			if _, ok := c.Secrets.Sets[ref.Set]; !ok {
				return nil, fmt.Errorf("repo %s: secret set %q has no delivery policy", c.Repos[i].Name, ref.Set)
			}
		}
		if err := validatePolicy(c.Repos[i].ToolPolicy); err != nil {
			return nil, fmt.Errorf("repo %s tool_policy: %w", c.Repos[i].Name, err)
		}
	}
	return &c, nil
}

func validatePolicy(policy adapter.ToolPolicy) error {
	if len(policy.NetworkAllow) != 0 {
		return fmt.Errorf("network_allow is not enforceable by the Phase 1 LocalDockerRunner; leave it empty until runner-level egress filtering lands")
	}
	for kind, patterns := range map[string][][]string{
		"allowed_commands": policy.AllowedCommands,
		"denied_commands":  policy.DeniedCommands,
	} {
		for i, pattern := range patterns {
			if len(pattern) == 0 {
				return fmt.Errorf("%s[%d] is empty", kind, i)
			}
			for _, token := range pattern {
				if token == "" || strings.ContainsRune(token, '\x00') {
					return fmt.Errorf("%s[%d] contains an empty or invalid token", kind, i)
				}
			}
		}
	}
	return nil
}

func (c *Config) SecretResolver() *secrets.LocalFileResolver {
	return &secrets.LocalFileResolver{
		Root:       c.Secrets.Root,
		Backend:    c.Secrets.Backend,
		SOPSBinary: c.Secrets.SOPSBinary,
		SOPSConfig: c.Secrets.SOPSConfig,
	}
}

func (c *Config) SecretPolicies() map[string]secrets.SetPolicy {
	policies := make(map[string]secrets.SetPolicy, len(c.Secrets.Sets))
	for name, policy := range c.Secrets.Sets {
		policies[c.Workspace+"/"+name] = secrets.SetPolicy{LocalEligible: policy.LocalEligible}
	}
	return policies
}

func (c *Config) Repo(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}

func (c *Config) RepoNames() []string {
	names := make([]string, len(c.Repos))
	for i, r := range c.Repos {
		names[i] = r.Name
	}
	return names
}

func expandDefault(v, home, def string) string {
	if v == "" {
		return def
	}
	if v == "~" || strings.HasPrefix(v, "~/") {
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(v, "~"), "/"))
	}
	return v
}

func safePathSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}
