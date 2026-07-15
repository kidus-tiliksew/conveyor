// Package config loads Conveyor's immutable deployment settings and the
// mutable workspace document. Phase 4.7 deliberately keeps execution
// credentials out of both documents: conveyord uses CONVEYOR_API_KEY and MCP
// clients bring their own agent credentials (spec §21.4).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Repo struct {
	Name   string `yaml:"name" json:"name"`
	URL    string `yaml:"url" json:"url"`
	GitHub string `yaml:"github,omitempty" json:"github,omitempty"`
	Base   string `yaml:"base" json:"base"`

	// Accepted only while canonicalizing pre-4.7 stored rows. These fields
	// are cleared by normalize and never cross the workspace API boundary.
	LegacyImage      string         `yaml:"image,omitempty" json:"-"`
	LegacySecretRefs []string       `yaml:"secret_refs,omitempty" json:"-"`
	LegacyToolPolicy map[string]any `yaml:"tool_policy,omitempty" json:"-"`
}

type Database struct {
	Backend string `yaml:"backend"`
	URL     string `yaml:"url"`
}

type ExecutionMode string

const (
	ExecutionInProcess ExecutionMode = "in_process"
	ExecutionMCP       ExecutionMode = "mcp"
)

type StageRoute struct {
	Model       string        `yaml:"model" json:"model"`
	Timeout     time.Duration `yaml:"-" json:"-"`
	TimeoutText string        `yaml:"timeout" json:"timeout"`
	Execution   ExecutionMode `yaml:"execution" json:"execution"`

	// v1.3 compatibility inputs. They are consumed during normalization and
	// omitted from the canonical v1.4 document.
	LegacyHarnesses []string `yaml:"harnesses,omitempty" json:"-"`
	LegacyModelTier string   `yaml:"model_tier,omitempty" json:"-"`
}

const (
	DefaultStageTimeout              = 2 * time.Hour
	DefaultWorkOrderQueueTimeout     = 24 * time.Hour
	DefaultWorkOrderQueueTimeoutText = "24h"
)

type Routing struct {
	Stages map[string]StageRoute `yaml:"stages" json:"stages"`
}

type WorkspaceDocument struct {
	Workspace                 string  `yaml:"workspace" json:"workspace"`
	MaxBounces                int     `yaml:"max_bounces" json:"max_bounces"`
	WorkOrderQueueTimeoutText string  `yaml:"work_order_queue_timeout" json:"work_order_queue_timeout"`
	Routing                   Routing `yaml:"routing" json:"routing"`
	Repos                     []Repo  `yaml:"repos" json:"repos"`

	// v1.3 compatibility input; never emitted after normalization.
	LegacyImage string `yaml:"image,omitempty" json:"-"`
}

var ErrVersionConflict = errors.New("workspace config version conflict")

type VersionedDocument struct {
	Document WorkspaceDocument `json:"document"`
	Version  int64             `json:"version"`
}

type UpdateReceipt struct {
	VersionedDocument
	EventID  int64    `json:"event_id"`
	ActorID  string   `json:"actor_id"`
	Sections []string `json:"sections"`
}

// Config combines immutable deployment settings with the current workspace
// snapshot. CacheDir is retained for the bare-clone cache and checkout flow.
type Config struct {
	Workspace                 string        `yaml:"workspace"`
	PackDir                   string        `yaml:"pack_dir"`
	MaxBounces                int           `yaml:"max_bounces"`
	WorkOrderQueueTimeout     time.Duration `yaml:"-"`
	WorkOrderQueueTimeoutText string        `yaml:"work_order_queue_timeout"`
	CacheDir                  string        `yaml:"cache_dir"`
	Database                  Database      `yaml:"database"`
	Routing                   Routing       `yaml:"routing"`
	Repos                     []Repo        `yaml:"repos"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := decodeKnown(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return normalize(&c, path)
}

func ParseWorkspaceDocument(data []byte, deployment *Config, source string) (*Config, error) {
	var document WorkspaceDocument
	if err := decodeKnown(data, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	if document.Workspace != "" && document.Workspace != deployment.Workspace {
		return nil, fmt.Errorf("workspace must remain %q", deployment.Workspace)
	}
	next := *deployment
	if document.Workspace == "" {
		document.Workspace = deployment.Workspace
	}
	next.Workspace = document.Workspace
	next.MaxBounces = document.MaxBounces
	next.WorkOrderQueueTimeoutText = document.WorkOrderQueueTimeoutText
	next.Routing = document.Routing
	next.Repos = document.Repos
	return normalize(&next, source)
}

// ParseStoredWorkspaceDocument canonicalizes both the v1.4 document and the
// superseded Phase 4.5 fields accepted by the compatibility members above.
func ParseStoredWorkspaceDocument(data []byte, deployment *Config, source string) (*Config, bool, error) {
	canonicalData, hadBudget, err := stripLegacyBudgetFields(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", source, err)
	}
	var document WorkspaceDocument
	if err := decodeKnown(canonicalData, &document); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", source, err)
	}
	legacy := hadBudget || document.LegacyImage != "" || document.WorkOrderQueueTimeoutText == ""
	for _, repo := range document.Repos {
		legacy = legacy || repo.LegacyImage != "" || len(repo.LegacySecretRefs) != 0 || len(repo.LegacyToolPolicy) != 0
	}
	for _, route := range document.Routing.Stages {
		legacy = legacy || len(route.LegacyHarnesses) != 0 || route.LegacyModelTier != ""
	}
	cfg, err := ParseWorkspaceDocument(canonicalData, deployment, source)
	return cfg, legacy, err
}

// stripLegacyBudgetFields lets an existing v1.5 Postgres workspace document
// boot once and be rewritten in the v1.6 shape (spec §21.6). New deployment
// files and API writes still reject budget_usd as an unknown field.
func stripLegacyBudgetFields(data []byte) ([]byte, bool, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, false, err
	}
	document := &root
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		document = root.Content[0]
	}
	routing := mappingValue(document, "routing")
	stages := mappingValue(routing, "stages")
	removed := false
	if stages != nil {
		for i := 1; i < len(stages.Content); i += 2 {
			removed = removeDirectMappingKey(stages.Content[i], "budget_usd") || removed
		}
	}
	if !removed {
		return data, false, nil
	}
	canonical, err := yaml.Marshal(&root)
	return canonical, true, err
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func removeDirectMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return true
		}
	}
	return false
}

func decodeKnown(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}

func normalize(c *Config, path string) (*Config, error) {
	if c.PackDir == "" {
		c.PackDir = "pack"
	}
	if !filepath.IsAbs(c.PackDir) {
		configDir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		c.PackDir = filepath.Join(configDir, c.PackDir)
	}
	if c.MaxBounces == 0 {
		c.MaxBounces = 2
	}
	if c.MaxBounces < 1 {
		return nil, fmt.Errorf("max_bounces must be at least 1")
	}
	if c.WorkOrderQueueTimeoutText == "" {
		c.WorkOrderQueueTimeout = DefaultWorkOrderQueueTimeout
		c.WorkOrderQueueTimeoutText = DefaultWorkOrderQueueTimeoutText
	} else {
		parsed, parseErr := time.ParseDuration(c.WorkOrderQueueTimeoutText)
		if parseErr != nil || parsed <= 0 {
			return nil, fmt.Errorf("work_order_queue_timeout must be a positive duration")
		}
		c.WorkOrderQueueTimeout = parsed
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c.CacheDir = expandDefault(c.CacheDir, home, filepath.Join(home, ".conveyor", "cache"))
	if c.Database.URL == "" {
		c.Database.URL = os.Getenv("CONVEYOR_DATABASE_URL")
	}
	if c.Database.Backend == "" {
		if c.Database.URL == "" {
			c.Database.Backend = "memory"
		} else {
			c.Database.Backend = "postgres"
		}
	}
	if c.Database.Backend != "postgres" && c.Database.Backend != "memory" {
		return nil, fmt.Errorf("database.backend must be %q or %q", "postgres", "memory")
	}
	if c.Database.Backend == "postgres" && c.Database.URL == "" {
		return nil, fmt.Errorf("database.url or CONVEYOR_DATABASE_URL is required for postgres backend")
	}
	for stage, route := range c.Routing.Stages {
		if route.Model == "" {
			route.Model = route.LegacyModelTier
		}
		if route.Model == "" {
			return nil, fmt.Errorf("routing stage %s: model is required", stage)
		}
		if route.TimeoutText == "" {
			route.Timeout = DefaultStageTimeout
			route.TimeoutText = DefaultStageTimeout.String()
		} else {
			parsed, parseErr := time.ParseDuration(route.TimeoutText)
			if parseErr != nil || parsed <= 0 {
				return nil, fmt.Errorf("routing stage %s: timeout must be a positive duration", stage)
			}
			route.Timeout = parsed
		}
		if route.Execution == "" {
			switch stage {
			case "triage", "spec":
				route.Execution = ExecutionInProcess
			default:
				route.Execution = ExecutionMCP
			}
		}
		if route.Execution != ExecutionInProcess && route.Execution != ExecutionMCP {
			return nil, fmt.Errorf("routing stage %s: execution must be in_process or mcp", stage)
		}
		if (stage == "triage" || stage == "spec") && route.Execution != ExecutionInProcess {
			return nil, fmt.Errorf("routing stage %s: execution is fixed to in_process", stage)
		}
		if stage == "implement" && route.Execution != ExecutionMCP {
			return nil, fmt.Errorf("routing stage implement: execution is fixed to mcp")
		}
		route.LegacyHarnesses = nil
		route.LegacyModelTier = ""
		c.Routing.Stages[stage] = route
	}
	for _, required := range []string{"triage", "spec", "implement", "review"} {
		if _, ok := c.Routing.Stages[required]; !ok {
			return nil, fmt.Errorf("routing stage %s is required", required)
		}
	}
	repoNames := make(map[string]struct{}, len(c.Repos))
	for i := range c.Repos {
		repo := &c.Repos[i]
		if repo.Name == "" || repo.URL == "" {
			return nil, fmt.Errorf("repo %d: name and url are required", i)
		}
		if _, duplicate := repoNames[repo.Name]; duplicate {
			return nil, fmt.Errorf("duplicate repo name %q", repo.Name)
		}
		repoNames[repo.Name] = struct{}{}
		if !safePathSegment(repo.Name) {
			return nil, fmt.Errorf("repo %d: name %q must be one path-safe segment", i, repo.Name)
		}
		if repo.Base == "" {
			repo.Base = "main"
		}
		repo.LegacyImage = ""
		repo.LegacySecretRefs = nil
		repo.LegacyToolPolicy = nil
	}
	return c, nil
}

func (c *Config) WorkspaceDocument() WorkspaceDocument {
	document := WorkspaceDocument{
		Workspace: c.Workspace, MaxBounces: c.MaxBounces,
		WorkOrderQueueTimeoutText: c.WorkOrderQueueTimeoutText,
		Routing:                   Routing{Stages: make(map[string]StageRoute, len(c.Routing.Stages))},
		Repos:                     append([]Repo(nil), c.Repos...),
	}
	for stage, route := range c.Routing.Stages {
		route.Timeout = 0
		route.LegacyHarnesses = nil
		route.LegacyModelTier = ""
		document.Routing.Stages[stage] = route
	}
	return document
}

func MarshalWorkspaceDocument(c *Config) ([]byte, error) {
	data, err := yaml.Marshal(c.WorkspaceDocument())
	if err != nil {
		return nil, fmt.Errorf("marshal workspace config: %w", err)
	}
	return data, nil
}

func (c *Config) Repo(name string) (Repo, bool) {
	for _, repo := range c.Repos {
		if repo.Name == name {
			return repo, true
		}
	}
	return Repo{}, false
}

func (c *Config) RepoNames() []string {
	names := make([]string, len(c.Repos))
	for i, repo := range c.Repos {
		names[i] = repo.Name
	}
	return names
}

func expandDefault(value, home, fallback string) string {
	if value == "" {
		return fallback
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/"))
	}
	return value
}

func safePathSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}
