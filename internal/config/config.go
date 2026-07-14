// Package config loads one workspace, runner paths, routing policy,
// credential metadata, the Phase 3 proto-pack, and repositories.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/secrets"
	"gopkg.in/yaml.v3"
)

type Repo struct {
	Name       string             `yaml:"name" json:"name"`
	URL        string             `yaml:"url" json:"url"`
	GitHub     string             `yaml:"github" json:"github,omitempty"` // owner/repo slug for gh; empty disables PR/issue flow
	Base       string             `yaml:"base" json:"base"`               // default base branch
	Image      string             `yaml:"image" json:"image"`             // per-repo sandbox image override (spec §19)
	SecretRefs []string           `yaml:"secret_refs" json:"secret_refs,omitempty"`
	ToolPolicy adapter.ToolPolicy `yaml:"tool_policy" json:"tool_policy"`
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

type Database struct {
	Backend string `yaml:"backend"` // postgres (Phase 2) | memory (explicit development mode)
	URL     string `yaml:"url"`     // prefer CONVEYOR_DATABASE_URL so credentials stay out of YAML
}

type Credential struct {
	ID        string `yaml:"id"`
	OwnerID   string `yaml:"owner_id"`
	OwnerKind string `yaml:"owner_kind"` // user | org
	Kind      string `yaml:"kind"`       // personal_sub | team_sub | api
	Vendor    string `yaml:"vendor"`
	Harness   string `yaml:"harness"` // codex | claude-code
	// Ref is a runner-local credential directory or a secretref:// URL. It is
	// metadata, never the credential value itself (spec §5.2).
	Ref string `yaml:"ref"`
}

type VendorPolicy struct {
	Vendor               string `yaml:"vendor"`
	Harness              string `yaml:"harness"`
	AuthMode             string `yaml:"auth_mode"`
	SubscriptionHeadless string `yaml:"subscription_headless"`
	ReviewedAt           string `yaml:"reviewed_at"`
	SourceURL            string `yaml:"source_url"`
}

type StageRoute struct {
	Harnesses   []string      `yaml:"harnesses" json:"harnesses"`
	ModelTier   string        `yaml:"model_tier" json:"model_tier,omitempty"`
	BudgetUSD   float64       `yaml:"budget_usd" json:"budget_usd"`
	Timeout     time.Duration `yaml:"-" json:"-"`
	TimeoutText string        `yaml:"timeout" json:"timeout"`
}

const DefaultStageTimeout = 2 * time.Hour

type Routing struct {
	OwnerID         string                `yaml:"owner_id" json:"owner_id,omitempty"`
	LeaseSeconds    int                   `yaml:"lease_seconds" json:"lease_seconds,omitempty"`
	AllowRestricted bool                  `yaml:"allow_restricted" json:"allow_restricted,omitempty"`
	Stages          map[string]StageRoute `yaml:"stages" json:"stages"`
}

type WorkspaceRouting struct {
	Stages map[string]StageRoute `yaml:"stages" json:"stages"`
}

// WorkspaceDocument is the mutable, Postgres-backed portion of Config. The
// deployment substrate, credential pool, vendor policy, and secret backend
// deliberately do not cross the workspace config API boundary (spec §21.3).
type WorkspaceDocument struct {
	Workspace  string           `yaml:"workspace" json:"workspace"`
	Image      string           `yaml:"image" json:"image"`
	MaxBounces int              `yaml:"max_bounces" json:"max_bounces"`
	Routing    WorkspaceRouting `yaml:"routing" json:"routing"`
	Repos      []Repo           `yaml:"repos" json:"repos"`
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

type Config struct {
	Workspace  string `yaml:"workspace"`
	Image      string `yaml:"image"`
	PackDir    string `yaml:"pack_dir"`
	MaxBounces int    `yaml:"max_bounces"`
	CacheDir   string `yaml:"cache_dir"`
	JobsDir    string `yaml:"jobs_dir"`
	// CodexCredentials is the host directory with the user's Codex
	// login, mounted read-only into sandboxes (spec §5.2).
	CodexCredentials string         `yaml:"codex_credentials"`
	Database         Database       `yaml:"database"`
	Secrets          Secrets        `yaml:"secrets"`
	Credentials      []Credential   `yaml:"credentials"`
	VendorPolicies   []VendorPolicy `yaml:"vendor_policies"`
	Routing          Routing        `yaml:"routing"`
	Repos            []Repo         `yaml:"repos"`
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

// ParseWorkspaceDocument applies a database/API document to the file-backed
// deployment settings and runs the exact same normalization and validation as
// Load. The workspace identity is immutable because durable tasks and repos
// are keyed by it (spec §16, §21.3).
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
	next.Image = document.Image
	next.MaxBounces = document.MaxBounces
	next.Routing = deployment.Routing
	next.Routing.Stages = document.Routing.Stages
	next.Repos = document.Repos
	return normalize(&next, source)
}

// ParseStoredWorkspaceDocument accepts both the Phase 4.5 workspace-only
// document and the legacy full sanitized Config written by Phase 2/3. The
// legacy path extracts only workspace scope; deployment and credential fields
// continue to come from the current file (spec §21.3).
func ParseStoredWorkspaceDocument(data []byte, deployment *Config, source string) (*Config, bool, error) {
	cfg, err := ParseWorkspaceDocument(data, deployment, source)
	if err == nil {
		return cfg, false, nil
	}
	var legacy Config
	if legacyErr := decodeKnown(data, &legacy); legacyErr != nil {
		return nil, false, err
	}
	legacyData, legacyErr := yaml.Marshal(legacy.WorkspaceDocument())
	if legacyErr != nil {
		return nil, false, legacyErr
	}
	cfg, legacyErr = ParseWorkspaceDocument(legacyData, deployment, source)
	if legacyErr != nil {
		return nil, false, legacyErr
	}
	return cfg, true, nil
}

func decodeKnown(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}

func normalize(c *Config, path string) (*Config, error) {
	if c.Workspace == "" {
		c.Workspace = "default"
	}
	if c.Image == "" {
		c.Image = "conveyor-base:dev"
	}
	if c.PackDir == "" {
		c.PackDir = "pack"
	}
	if !filepath.IsAbs(c.PackDir) {
		configDir, absErr := filepath.Abs(filepath.Dir(path))
		if absErr != nil {
			return nil, absErr
		}
		c.PackDir = filepath.Join(configDir, c.PackDir)
	}
	if c.MaxBounces == 0 {
		c.MaxBounces = 2
	}
	if c.MaxBounces < 1 {
		return nil, fmt.Errorf("max_bounces must be at least 1")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c.CacheDir = expandDefault(c.CacheDir, home, filepath.Join(home, ".conveyor", "cache"))
	c.JobsDir = expandDefault(c.JobsDir, home, filepath.Join(home, ".conveyor", "jobs"))
	c.CodexCredentials = expandDefault(c.CodexCredentials, home, filepath.Join(home, ".codex"))
	if c.Routing.OwnerID == "" {
		c.Routing.OwnerID = "local-operator"
	}
	if c.Routing.LeaseSeconds == 0 {
		c.Routing.LeaseSeconds = 4 * 60 * 60
	}
	if c.Routing.LeaseSeconds < 60 {
		return nil, fmt.Errorf("routing.lease_seconds must be at least 60")
	}
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
	credentialIDs := make(map[string]struct{}, len(c.Credentials))
	for i := range c.Credentials {
		credential := &c.Credentials[i]
		if credential.OwnerID == "" {
			credential.OwnerID = c.Routing.OwnerID
		}
		if credential.OwnerKind == "" {
			credential.OwnerKind = "user"
		}
		if credential.ID == "" || credential.Vendor == "" || credential.Harness == "" || credential.Ref == "" {
			return nil, fmt.Errorf("credential %d: id, vendor, harness, and ref are required", i)
		}
		if _, duplicate := credentialIDs[credential.ID]; duplicate {
			return nil, fmt.Errorf("duplicate credential id %q", credential.ID)
		}
		credentialIDs[credential.ID] = struct{}{}
		if credential.OwnerKind != "user" && credential.OwnerKind != "org" {
			return nil, fmt.Errorf("credential %s: owner_kind must be user or org", credential.ID)
		}
		if credential.Kind != "personal_sub" && credential.Kind != "team_sub" && credential.Kind != "api" {
			return nil, fmt.Errorf("credential %s: unsupported kind %q", credential.ID, credential.Kind)
		}
		if credential.Kind != "api" && credential.OwnerKind != "user" {
			return nil, fmt.Errorf("credential %s: subscription credentials must be owned by one user", credential.ID)
		}
		if credential.Harness != "codex" && credential.Harness != "claude-code" {
			return nil, fmt.Errorf("credential %s: unsupported harness %q", credential.ID, credential.Harness)
		}
		if strings.HasPrefix(credential.Ref, "secretref://") {
			ref, err := secrets.ParseRef(credential.Ref)
			if err != nil {
				return nil, fmt.Errorf("credential %s: %w", credential.ID, err)
			}
			if ref.Workspace != c.Workspace {
				return nil, fmt.Errorf("credential %s: secret workspace %q does not match %q", credential.ID, ref.Workspace, c.Workspace)
			}
			if _, ok := c.Secrets.Sets[ref.Set]; !ok {
				return nil, fmt.Errorf("credential %s: secret set %q has no delivery policy", credential.ID, ref.Set)
			}
			if !validHarnessCredentialEnv(credential.Harness, credential.Kind, ref.Name) {
				return nil, fmt.Errorf("credential %s: %s is not a supported %s %s environment credential", credential.ID, ref.Name, credential.Harness, credential.Kind)
			}
		} else {
			credential.Ref = expandDefault(credential.Ref, home, "")
		}
	}
	for i, policy := range c.VendorPolicies {
		if policy.Vendor == "" || policy.Harness == "" || policy.AuthMode == "" || policy.SourceURL == "" {
			return nil, fmt.Errorf("vendor policy %d: vendor, harness, auth_mode, and source_url are required", i)
		}
		switch policy.SubscriptionHeadless {
		case "allowed", "restricted", "disallowed", "unknown":
		default:
			return nil, fmt.Errorf("vendor policy %d: invalid subscription_headless %q", i, policy.SubscriptionHeadless)
		}
		if _, err := time.Parse("2006-01-02", policy.ReviewedAt); err != nil {
			return nil, fmt.Errorf("vendor policy %d: reviewed_at must be YYYY-MM-DD", i)
		}
	}
	for stage, route := range c.Routing.Stages {
		if len(route.Harnesses) == 0 {
			return nil, fmt.Errorf("routing stage %s: at least one harness is required", stage)
		}
		for _, harness := range route.Harnesses {
			if harness != "codex" && harness != "claude-code" {
				return nil, fmt.Errorf("routing stage %s: unsupported harness %q", stage, harness)
			}
		}
		if route.BudgetUSD < 0 {
			return nil, fmt.Errorf("routing stage %s: budget_usd cannot be negative", stage)
		}
		if route.TimeoutText == "" {
			route.Timeout = DefaultStageTimeout
		} else {
			parsed, err := time.ParseDuration(route.TimeoutText)
			if err != nil || parsed <= 0 {
				return nil, fmt.Errorf("routing stage %s: timeout must be a positive duration", stage)
			}
			route.Timeout = parsed
		}
		c.Routing.Stages[stage] = route
	}
	if len(c.Credentials) > 0 && c.Database.Backend != "postgres" {
		return nil, fmt.Errorf("credential routing requires the postgres database backend")
	}
	repoNames := make(map[string]struct{}, len(c.Repos))
	for i := range c.Repos {
		if c.Repos[i].Name == "" || c.Repos[i].URL == "" {
			return nil, fmt.Errorf("repo %d: name and url are required", i)
		}
		if _, duplicate := repoNames[c.Repos[i].Name]; duplicate {
			return nil, fmt.Errorf("duplicate repo name %q", c.Repos[i].Name)
		}
		repoNames[c.Repos[i].Name] = struct{}{}
		if !safePathSegment(c.Repos[i].Name) {
			return nil, fmt.Errorf("repo %d: name %q must be one path-safe segment", i, c.Repos[i].Name)
		}
		if c.Repos[i].Base == "" {
			c.Repos[i].Base = "main"
		}
		if c.Repos[i].Image == "" {
			c.Repos[i].Image = c.Image
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
	return c, nil
}

// WorkspaceDocument returns a detached copy of the database-backed scope.
// Runtime-only parsed durations are omitted; TimeoutText is the wire value.
func (c *Config) WorkspaceDocument() WorkspaceDocument {
	document := WorkspaceDocument{
		Workspace: c.Workspace, Image: c.Image, MaxBounces: c.MaxBounces,
		Routing: WorkspaceRouting{Stages: make(map[string]StageRoute, len(c.Routing.Stages))},
		Repos:   make([]Repo, len(c.Repos)),
	}
	for stage, route := range c.Routing.Stages {
		route.Harnesses = append([]string(nil), route.Harnesses...)
		route.Timeout = 0
		document.Routing.Stages[stage] = route
	}
	for i, repo := range c.Repos {
		repo.SecretRefs = append([]string(nil), repo.SecretRefs...)
		repo.ToolPolicy.AllowedCommands = cloneCommands(repo.ToolPolicy.AllowedCommands)
		repo.ToolPolicy.DeniedCommands = cloneCommands(repo.ToolPolicy.DeniedCommands)
		repo.ToolPolicy.NetworkAllow = append([]string(nil), repo.ToolPolicy.NetworkAllow...)
		document.Repos[i] = repo
	}
	return document
}

func cloneCommands(commands [][]string) [][]string {
	cloned := make([][]string, len(commands))
	for i := range commands {
		cloned[i] = append([]string(nil), commands[i]...)
	}
	return cloned
}

func MarshalWorkspaceDocument(c *Config) ([]byte, error) {
	data, err := yaml.Marshal(c.WorkspaceDocument())
	if err != nil {
		return nil, fmt.Errorf("marshal workspace config: %w", err)
	}
	return data, nil
}

func validHarnessCredentialEnv(harness, kind, name string) bool {
	switch harness {
	case "codex":
		return kind == "api" && name == "OPENAI_API_KEY"
	case "claude-code":
		if kind == "api" {
			return name == "ANTHROPIC_API_KEY" || name == "ANTHROPIC_AUTH_TOKEN"
		}
		return name == "CLAUDE_CODE_OAUTH_TOKEN"
	default:
		return false
	}
}

func validatePolicy(policy adapter.ToolPolicy) error {
	if len(policy.NetworkAllow) != 0 {
		return fmt.Errorf("network_allow is not enforceable by LocalDockerRunner; leave it empty until runner-level egress filtering lands")
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
