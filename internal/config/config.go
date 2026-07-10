// Package config loads the deployment configuration for Phase 1: one
// workspace, its repos, and host paths. The full layered scope model
// (platform → workspace → task, spec §2.1) arrives with the pack
// machinery; this file is deliberately just enough to run the loop.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Repo struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	GitHub string `yaml:"github"` // owner/repo slug for gh; empty disables PR/issue flow
	Base   string `yaml:"base"`   // default base branch
}

type Config struct {
	Workspace string `yaml:"workspace"`
	Image     string `yaml:"image"`
	CacheDir  string `yaml:"cache_dir"`
	JobsDir   string `yaml:"jobs_dir"`
	// CodexCredentials is the host directory with the user's Codex
	// login, mounted read-only into sandboxes (spec §5.2).
	CodexCredentials string `yaml:"codex_credentials"`
	Repos            []Repo `yaml:"repos"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
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
	for i := range c.Repos {
		if c.Repos[i].Name == "" || c.Repos[i].URL == "" {
			return nil, fmt.Errorf("repo %d: name and url are required", i)
		}
		if c.Repos[i].Base == "" {
			c.Repos[i].Base = "main"
		}
	}
	return &c, nil
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
