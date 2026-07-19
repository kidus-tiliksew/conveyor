// Package config loads Conveyor's immutable deployment settings and the
// mutable workspace document. Phase 4.7 deliberately keeps execution
// credentials out of both documents: conveyord uses CONVEYOR_API_KEY and MCP
// clients bring their own agent credentials (spec §21.4).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	ModelPolicy string        `yaml:"model_policy,omitempty" json:"model_policy,omitempty"`
	Harness     string        `yaml:"harness,omitempty" json:"harness,omitempty"`
	Effort      string        `yaml:"effort,omitempty" json:"effort,omitempty"`
	Timeout     time.Duration `yaml:"-" json:"-"`
	TimeoutText string        `yaml:"timeout" json:"timeout"`
	Execution   ExecutionMode `yaml:"execution" json:"execution"`
	// EffectiveModel is the normalized worker argument. It is deliberately
	// absent from persisted compatibility routes (spec §21.18 changes 2-3).
	EffectiveModel string `yaml:"-" json:"-"`

	// v1.3 compatibility inputs. They are consumed during normalization and
	// omitted from the canonical v1.4 document.
	LegacyHarnesses []string `yaml:"harnesses,omitempty" json:"-"`
	LegacyModelTier string   `yaml:"model_tier,omitempty" json:"-"`
}

type Harness struct {
	Name                  string              `yaml:"name" json:"name"`
	MCPTransport          string              `yaml:"mcp_transport" json:"mcp_transport"`
	MCPAttachment         string              `yaml:"mcp_attachment,omitempty" json:"mcp_attachment,omitempty"`
	Command               []string            `yaml:"command" json:"command"`
	ModelArgs             []string            `yaml:"model_args,omitempty" json:"model_args,omitempty"`
	DefaultModelSentinels []string            `yaml:"default_model_sentinels,omitempty" json:"default_model_sentinels,omitempty"`
	EffortArgs            map[string][]string `yaml:"effort_args,omitempty" json:"effort_args,omitempty"`
	ProbeCommand          []string            `yaml:"probe_command" json:"probe_command"`
	ProbeTimeout          time.Duration       `yaml:"-" json:"-"`
	ProbeTimeoutText      string              `yaml:"probe_timeout" json:"probe_timeout"`
}

const (
	// MCP transport controls the representation substituted for the whole
	// {mcp_config} argv element (spec §21.20).
	MCPTransportJSONFile     = "json_file"
	MCPTransportTOMLOverride = "toml_override"
	MCPTransportEnvironment  = "environment"
)

var mcpAttachmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ReviewSeat is one immutable assignment in a submitted review round. The
// model is always pinned; Harness optionally overrides the workspace review
// route for worker dispatch (spec §21.12 change 4).
type ReviewSeat struct {
	Model   string `yaml:"model" json:"model"`
	Harness string `yaml:"harness,omitempty" json:"harness,omitempty"`
	Effort  string `yaml:"effort,omitempty" json:"effort,omitempty"`
}

type ReviewPanel struct {
	Seats []ReviewSeat `yaml:"seats,omitempty" json:"seats"`
}

type ExecutionPolicy struct {
	DefaultMode          string `yaml:"default_mode" json:"default_mode"`
	SpecApproval         bool   `yaml:"spec_approval" json:"spec_approval"`
	MergeApproval        bool   `yaml:"merge_approval" json:"merge_approval"`
	ImplementConcurrency int    `yaml:"implement_concurrency" json:"implement_concurrency"`
	ReviewConcurrency    int    `yaml:"review_concurrency" json:"review_concurrency"`
}

const (
	DefaultStageTimeout              = 2 * time.Hour
	DefaultWorkOrderQueueTimeout     = 24 * time.Hour
	DefaultWorkOrderQueueTimeoutText = "24h"
)

type Routing struct {
	Stages map[string]StageRoute `yaml:"stages" json:"stages"`
}

const (
	ModelPolicyExplicit       = "explicit"
	ModelPolicyHarnessDefault = "harness_default"
)

type ModelTimeoutSettings struct {
	Model       string `yaml:"model" json:"model"`
	TimeoutText string `yaml:"timeout" json:"timeout"`
}

type ControlPlaneSettings struct {
	Triage ModelTimeoutSettings `yaml:"triage" json:"triage"`
	Spec   ModelTimeoutSettings `yaml:"spec" json:"spec"`
}

type ImplementationSettings struct {
	Harness     string `yaml:"harness" json:"harness"`
	Model       string `yaml:"model,omitempty" json:"model,omitempty"`
	ModelPolicy string `yaml:"model_policy" json:"model_policy"`
	Effort      string `yaml:"effort,omitempty" json:"effort,omitempty"`
	TimeoutText string `yaml:"timeout" json:"timeout"`
}

type ReviewExecutionSettings struct {
	Execution       ExecutionMode `yaml:"execution" json:"execution"`
	TimeoutText     string        `yaml:"timeout" json:"timeout"`
	FallbackModel   string        `yaml:"fallback_model,omitempty" json:"fallback_model,omitempty"`
	FallbackHarness string        `yaml:"fallback_harness,omitempty" json:"fallback_harness,omitempty"`
}

// ContextualExecutionSettings is the v1.18 canonical surface. Routing remains
// additive compatibility data and is never a second source of truth when this
// object is present (spec §21.18 changes 1-2).
type ContextualExecutionSettings struct {
	ControlPlane   ControlPlaneSettings    `yaml:"control_plane" json:"control_plane"`
	Implementation ImplementationSettings  `yaml:"implementation" json:"implementation"`
	Review         ReviewExecutionSettings `yaml:"review" json:"review"`
}

type WorkspaceDocument struct {
	Workspace                 string                       `yaml:"workspace" json:"workspace"`
	MaxBounces                int                          `yaml:"max_bounces" json:"max_bounces"`
	WorkOrderQueueTimeoutText string                       `yaml:"work_order_queue_timeout" json:"work_order_queue_timeout"`
	ExecutionSettings         *ContextualExecutionSettings `yaml:"execution_settings,omitempty" json:"execution_settings,omitempty"`
	Routing                   Routing                      `yaml:"routing" json:"routing"`
	Harnesses                 []Harness                    `yaml:"harnesses,omitempty" json:"harnesses"`
	Review                    ReviewPanel                  `yaml:"review" json:"review"`
	Execution                 ExecutionPolicy              `yaml:"execution" json:"execution"`
	Repos                     []Repo                       `yaml:"repos" json:"repos"`

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
	Workspace                 string                       `yaml:"workspace"`
	PackDir                   string                       `yaml:"pack_dir"`
	MaxBounces                int                          `yaml:"max_bounces"`
	WorkOrderQueueTimeout     time.Duration                `yaml:"-"`
	WorkOrderQueueTimeoutText string                       `yaml:"work_order_queue_timeout"`
	CacheDir                  string                       `yaml:"cache_dir"`
	Database                  Database                     `yaml:"database"`
	ExecutionSettings         *ContextualExecutionSettings `yaml:"execution_settings,omitempty"`
	Routing                   Routing                      `yaml:"routing"`
	Harnesses                 []Harness                    `yaml:"harnesses,omitempty"`
	Review                    ReviewPanel                  `yaml:"review"`
	Execution                 ExecutionPolicy              `yaml:"execution"`
	Repos                     []Repo                       `yaml:"repos"`
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
	next.ExecutionSettings = document.ExecutionSettings
	next.Routing = document.Routing
	next.Harnesses = document.Harnesses
	next.Review = document.Review
	next.Execution = document.Execution
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
	legacy := hadBudget || document.LegacyImage != "" || document.WorkOrderQueueTimeoutText == "" || document.Review.Seats == nil || document.ExecutionSettings == nil
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

func applyContextualExecutionSettings(c *Config) {
	if c.ExecutionSettings == nil {
		return
	}
	settings := c.ExecutionSettings
	if c.Routing.Stages == nil {
		c.Routing.Stages = map[string]StageRoute{}
	}
	c.Routing.Stages["triage"] = StageRoute{
		Model: settings.ControlPlane.Triage.Model, TimeoutText: settings.ControlPlane.Triage.TimeoutText,
		Execution: ExecutionInProcess, ModelPolicy: ModelPolicyExplicit,
	}
	c.Routing.Stages["spec"] = StageRoute{
		Model: settings.ControlPlane.Spec.Model, TimeoutText: settings.ControlPlane.Spec.TimeoutText,
		Execution: ExecutionInProcess, ModelPolicy: ModelPolicyExplicit,
	}
	c.Routing.Stages["implement"] = StageRoute{
		Model: settings.Implementation.Model, ModelPolicy: settings.Implementation.ModelPolicy,
		Harness: settings.Implementation.Harness, Effort: settings.Implementation.Effort,
		TimeoutText: settings.Implementation.TimeoutText,
		Execution:   ExecutionMCP,
	}
	c.Routing.Stages["review"] = StageRoute{
		Model: settings.Review.FallbackModel, Harness: settings.Review.FallbackHarness,
		TimeoutText: settings.Review.TimeoutText, Execution: settings.Review.Execution,
		ModelPolicy: ModelPolicyExplicit,
	}
}

func contextualExecutionSettings(routing Routing) *ContextualExecutionSettings {
	triage := routing.Stages["triage"]
	spec := routing.Stages["spec"]
	implement := routing.Stages["implement"]
	review := routing.Stages["review"]
	return &ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage: ModelTimeoutSettings{Model: triage.Model, TimeoutText: triage.TimeoutText},
			Spec:   ModelTimeoutSettings{Model: spec.Model, TimeoutText: spec.TimeoutText},
		},
		Implementation: ImplementationSettings{
			Harness: implement.Harness, Model: implement.Model,
			ModelPolicy: implement.ModelPolicy, Effort: implement.Effort,
			TimeoutText: implement.TimeoutText,
		},
		Review: ReviewExecutionSettings{
			Execution: review.Execution, TimeoutText: review.TimeoutText,
			FallbackModel: review.Model, FallbackHarness: review.Harness,
		},
	}
}

func symbolicModelPolicy(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "subscription", "operator", "operator-owned", "default", "harness-default":
		return true
	default:
		return false
	}
}

func normalizeHarnessModel(route StageRoute, harnesses []Harness) (string, error) {
	symbol := strings.TrimSpace(route.Model)
	if route.ModelPolicy == ModelPolicyExplicit {
		if symbol == "" {
			return "", fmt.Errorf("explicit model policy requires a model")
		}
		if symbolicModelPolicy(symbol) {
			return "", fmt.Errorf("symbolic model %q requires harness_default model policy", symbol)
		}
		return symbol, nil
	}
	var selected *Harness
	for i := range harnesses {
		if harnesses[i].Name == route.Harness {
			selected = &harnesses[i]
			break
		}
	}
	if selected == nil {
		// Legacy/manual-only documents may predate the harness registry. They
		// remain readable; worker health rejects Auto because no route harness
		// is usable (spec §21.18 changes 2 and 6).
		if route.Harness == "" {
			return "", nil
		}
		return "", fmt.Errorf("harness_default model policy requires a usable harness")
	}
	if symbol == "" {
		return "", nil
	}
	for _, supported := range selected.DefaultModelSentinels {
		if symbol == supported {
			return symbol, nil
		}
	}
	if symbolicModelPolicy(symbol) {
		return "", nil
	}
	return "", fmt.Errorf("model %q is not declared by harness %q as a default-model sentinel", symbol, selected.Name)
}

func normalize(c *Config, path string) (*Config, error) {
	applyContextualExecutionSettings(c)
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
		c.MaxBounces = 10
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
	if c.Execution.DefaultMode == "" {
		c.Execution.DefaultMode = "auto"
		c.Execution.SpecApproval = true
		c.Execution.MergeApproval = true
	}
	if c.Execution.DefaultMode != "auto" && c.Execution.DefaultMode != "manual" {
		return nil, fmt.Errorf("execution.default_mode must be auto or manual")
	}
	if c.Execution.ImplementConcurrency == 0 {
		c.Execution.ImplementConcurrency = 1
	}
	if c.Execution.ReviewConcurrency == 0 {
		c.Execution.ReviewConcurrency = 1
	}
	if c.Execution.ImplementConcurrency < 1 {
		return nil, fmt.Errorf("execution.implement_concurrency must be at least 1")
	}
	if c.Execution.ReviewConcurrency < 1 {
		return nil, fmt.Errorf("execution.review_concurrency must be at least 1")
	}
	harnesses := make(map[string]Harness, len(c.Harnesses))
	for i := range c.Harnesses {
		harness := &c.Harnesses[i]
		if strings.TrimSpace(harness.Name) == "" || !safePathSegment(harness.Name) {
			return nil, fmt.Errorf("harnesses[%d].name must be one path-safe segment", i)
		}
		if _, duplicate := harnesses[harness.Name]; duplicate {
			return nil, fmt.Errorf("duplicate harness name %q", harness.Name)
		}
		if harness.MCPTransport == "" {
			harness.MCPTransport = MCPTransportJSONFile
		}
		harness.MCPAttachment = strings.TrimSpace(harness.MCPAttachment)
		harnesses[harness.Name] = *harness
		if err := validateHarness(*harness, i); err != nil {
			return nil, err
		}
		parsed, _ := time.ParseDuration(harness.ProbeTimeoutText)
		harness.ProbeTimeout = parsed
	}
	for stage, route := range c.Routing.Stages {
		if stage != "implement" && strings.TrimSpace(route.Effort) != "" {
			return nil, fmt.Errorf("routing stage %s: effort is valid only for implementation", stage)
		}
		if route.Model == "" {
			route.Model = route.LegacyModelTier
		}
		if route.ModelPolicy == "" {
			if stage == "implement" && symbolicModelPolicy(route.Model) {
				route.ModelPolicy = ModelPolicyHarnessDefault
			} else {
				route.ModelPolicy = ModelPolicyExplicit
			}
		}
		if route.ModelPolicy != ModelPolicyExplicit && route.ModelPolicy != ModelPolicyHarnessDefault {
			return nil, fmt.Errorf("routing stage %s: model_policy must be explicit or harness_default", stage)
		}
		if route.Model == "" && (stage == "triage" || stage == "spec" || stage == "implement" && route.ModelPolicy == ModelPolicyExplicit) {
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
		if stage == "triage" || stage == "spec" {
			// Harness fields on pre-v1.18 control-plane routes are compatibility
			// noise and never become worker requirements (spec §21.18 change 2).
			route.Harness = ""
		}
		if stage == "review" && route.Execution == ExecutionInProcess {
			if route.Harness != "" {
				return nil, fmt.Errorf("routing stage review: in_process execution cannot select a harness")
			}
		} else if stage == "implement" {
			route.Effort = strings.TrimSpace(route.Effort)
			if route.Harness == "" && len(c.Harnesses) == 1 {
				route.Harness = c.Harnesses[0].Name
			}
			if route.Harness == "" && len(c.Harnesses) > 1 {
				return nil, fmt.Errorf("routing stage %s: harness is required when multiple harnesses are registered", stage)
			}
			var implementationHarness Harness
			if route.Harness != "" {
				var ok bool
				if implementationHarness, ok = harnesses[route.Harness]; !ok {
					return nil, fmt.Errorf("routing stage %s: unknown harness %q", stage, route.Harness)
				}
			}
			if route.Effort != "" {
				if !validEffort(route.Effort) {
					return nil, fmt.Errorf("execution_settings.implementation.effort must be low, medium, or high")
				}
				if len(implementationHarness.EffortArgs[route.Effort]) == 0 {
					return nil, fmt.Errorf("execution_settings.implementation.effort %q is not supported by harness %q", route.Effort, route.Harness)
				}
			}
			effective, modelErr := normalizeHarnessModel(route, c.Harnesses)
			if modelErr != nil {
				return nil, fmt.Errorf("routing stage implement: %w", modelErr)
			}
			route.EffectiveModel = effective
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
	reviewRoute := c.Routing.Stages["review"]
	if c.Review.Seats == nil {
		// Upgrade the pre-5.2 single review route without changing its behavior.
		c.Review.Seats = []ReviewSeat{{Model: reviewRoute.Model, Harness: reviewRoute.Harness}}
	}
	if len(c.Review.Seats) == 0 {
		return nil, fmt.Errorf("review.seats must contain at least one seat")
	}
	if reviewRoute.Execution == ExecutionInProcess && len(c.Review.Seats) != 1 {
		return nil, fmt.Errorf("review.seats must contain exactly one seat for in_process review execution")
	}
	for i, seat := range c.Review.Seats {
		seat.Model = strings.TrimSpace(seat.Model)
		seat.Harness = strings.TrimSpace(seat.Harness)
		seat.Effort = strings.TrimSpace(seat.Effort)
		if seat.Model == "" {
			return nil, fmt.Errorf("review.seats[%d].model is required", i)
		}
		if reviewRoute.Execution == ExecutionInProcess && seat.Harness != "" {
			return nil, fmt.Errorf("review.seats[%d].harness cannot override an in_process review route", i)
		}
		if seat.Harness != "" {
			if _, ok := harnesses[seat.Harness]; !ok {
				return nil, fmt.Errorf("review.seats[%d].harness references unknown harness %q", i, seat.Harness)
			}
		}
		if seat.Effort != "" {
			if !validEffort(seat.Effort) {
				return nil, fmt.Errorf("review.seats[%d].effort must be low, medium, or high", i)
			}
			if reviewRoute.Execution == ExecutionInProcess {
				return nil, fmt.Errorf("review.seats[%d].effort cannot override an in_process review route", i)
			}
			harnessName := seat.Harness
			if harnessName == "" {
				harnessName = reviewRoute.Harness
			}
			if harnessName == "" && len(c.Harnesses) == 1 {
				harnessName = c.Harnesses[0].Name
			}
			harness, ok := harnesses[harnessName]
			if !ok || len(harness.EffortArgs[seat.Effort]) == 0 {
				return nil, fmt.Errorf("review.seats[%d].effort %q is not supported by harness %q", i, seat.Effort, harnessName)
			}
		}
		c.Review.Seats[i] = seat
	}
	if reviewRoute.Execution == ExecutionMCP {
		needsFallback := false
		for _, seat := range c.Review.Seats {
			needsFallback = needsFallback || seat.Harness == ""
		}
		if needsFallback {
			if reviewRoute.Harness == "" && len(c.Harnesses) == 1 {
				reviewRoute.Harness = c.Harnesses[0].Name
			}
			if reviewRoute.Harness == "" && len(c.Harnesses) > 0 {
				return nil, fmt.Errorf("routing stage review: fallback harness is required for seats without a harness override")
			}
			if reviewRoute.Harness != "" {
				if _, ok := harnesses[reviewRoute.Harness]; ok {
					c.Routing.Stages["review"] = reviewRoute
				} else {
					return nil, fmt.Errorf("routing stage review: unknown harness %q for fallback", reviewRoute.Harness)
				}
			}
		}
		// A stale fallback is intentionally not validated when every seat is
		// explicit; it is retained only for compatibility (spec §21.18 change 4).
		c.Routing.Stages["review"] = reviewRoute
	}
	c.ExecutionSettings = contextualExecutionSettings(c.Routing)
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
	reviewSeats := append(make([]ReviewSeat, 0, len(c.Review.Seats)), c.Review.Seats...)
	if len(reviewSeats) == 0 {
		if route, ok := c.Routing.Stages["review"]; ok && route.Model != "" {
			reviewSeats = append(reviewSeats, ReviewSeat{Model: route.Model, Harness: route.Harness})
		}
	}
	document := WorkspaceDocument{
		Workspace: c.Workspace, MaxBounces: c.MaxBounces,
		WorkOrderQueueTimeoutText: c.WorkOrderQueueTimeoutText,
		ExecutionSettings:         contextualExecutionSettings(c.Routing),
		Routing:                   Routing{Stages: make(map[string]StageRoute, len(c.Routing.Stages))},
		Harnesses:                 append(make([]Harness, 0, len(c.Harnesses)), c.Harnesses...),
		Review:                    ReviewPanel{Seats: reviewSeats},
		Execution:                 c.Execution,
		Repos:                     append(make([]Repo, 0, len(c.Repos)), c.Repos...),
	}
	for stage, route := range c.Routing.Stages {
		route.Timeout = 0
		route.EffectiveModel = ""
		route.LegacyHarnesses = nil
		route.LegacyModelTier = ""
		document.Routing.Stages[stage] = route
	}
	return document
}

func (c *Config) EffectiveModel(stage string) string {
	route := c.Routing.Stages[stage]
	if route.EffectiveModel != "" || route.ModelPolicy == ModelPolicyHarnessDefault {
		return route.EffectiveModel
	}
	return route.Model
}

func validateHarness(h Harness, index int) error {
	field := func(name string, values []string, allowed map[string]bool) (map[string]int, error) {
		counts := map[string]int{}
		for _, value := range values {
			if !strings.Contains(value, "{") && !strings.Contains(value, "}") {
				continue
			}
			if !allowed[value] {
				return nil, fmt.Errorf("harnesses[%d].%s: placeholder %q is not allowed as a whole argv element", index, name, value)
			}
			counts[value]++
		}
		return counts, nil
	}
	if len(h.Command) == 0 {
		return fmt.Errorf("harnesses[%d].command is required", index)
	}
	if h.MCPTransport != MCPTransportJSONFile && h.MCPTransport != MCPTransportTOMLOverride && h.MCPTransport != MCPTransportEnvironment {
		return fmt.Errorf("harnesses[%d].mcp_transport must be %q, %q, or %q", index, MCPTransportJSONFile, MCPTransportTOMLOverride, MCPTransportEnvironment)
	}
	counts, err := field("command", h.Command, map[string]bool{"{prompt}": true, "{mcp_config}": true})
	if err != nil {
		return err
	}
	if counts["{prompt}"] != 1 {
		return fmt.Errorf("harnesses[%d].command must contain exactly one {prompt}", index)
	}
	if h.MCPTransport == MCPTransportEnvironment {
		if counts["{mcp_config}"] != 0 {
			return fmt.Errorf("harnesses[%d].command must not contain {mcp_config} for environment transport", index)
		}
		if !mcpAttachmentPattern.MatchString(h.MCPAttachment) {
			return fmt.Errorf("harnesses[%d].mcp_attachment must be a non-secret MCP server name for environment transport", index)
		}
		if environmentHarnessContainsRuntimeValue(h) {
			return fmt.Errorf("harnesses[%d] environment transport fields must not contain runtime Conveyor credentials or addresses", index)
		}
	} else {
		if counts["{mcp_config}"] != 1 {
			return fmt.Errorf("harnesses[%d].command must contain exactly one {mcp_config} for %s transport", index, h.MCPTransport)
		}
		if h.MCPAttachment != "" {
			return fmt.Errorf("harnesses[%d].mcp_attachment is only valid for environment transport", index)
		}
	}
	if _, err = field("model_args", h.ModelArgs, map[string]bool{"{model}": true}); err != nil {
		return err
	}
	for effort, values := range h.EffortArgs {
		if !validEffort(effort) {
			return fmt.Errorf("harnesses[%d].effort_args contains unsupported effort %q", index, effort)
		}
		if len(values) == 0 {
			return fmt.Errorf("harnesses[%d].effort_args.%s must contain at least one argv element", index, effort)
		}
		if _, err = field("effort_args."+effort, values, map[string]bool{}); err != nil {
			return err
		}
	}
	if len(h.ProbeCommand) == 0 {
		return fmt.Errorf("harnesses[%d].probe_command is required", index)
	}
	if _, err = field("probe_command", h.ProbeCommand, map[string]bool{}); err != nil {
		return err
	}
	parsed, parseErr := time.ParseDuration(h.ProbeTimeoutText)
	if parseErr != nil || parsed <= 0 {
		return fmt.Errorf("harnesses[%d].probe_timeout must be a positive duration", index)
	}
	return nil
}

// ValidateHarness applies the same transport-aware durable contract to worker
// snapshots immediately before probing or launch (spec §21.28 changes 2, 5).
func ValidateHarness(h Harness) error {
	return validateHarness(h, 0)
}

func environmentHarnessContainsRuntimeValue(h Harness) bool {
	forbidden := func(value string) bool {
		upper := strings.ToUpper(value)
		if strings.Contains(upper, "CONVEYOR_ADDR") ||
			strings.Contains(upper, "CONVEYOR_API_TOKEN") ||
			strings.Contains(upper, "CONVEYOR_SESSION_ID") ||
			strings.Contains(upper, "CONVEYOR_CLIENT_TOKEN") ||
			strings.Contains(upper, "BEARER ") {
			return true
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return false
		}
		if parsed.User != nil {
			return true
		}
		for name := range parsed.Query() {
			name = strings.ToUpper(name)
			if strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "KEY") || strings.Contains(name, "AUTH") {
				return true
			}
		}
		return false
	}
	if forbidden(h.MCPAttachment) {
		return true
	}
	groups := [][]string{h.Command, h.ModelArgs, h.DefaultModelSentinels, h.ProbeCommand}
	for _, args := range h.EffortArgs {
		groups = append(groups, args)
	}
	for _, values := range groups {
		for _, value := range values {
			if forbidden(value) {
				return true
			}
		}
	}
	return false
}

func validEffort(effort string) bool {
	return effort == "low" || effort == "medium" || effort == "high"
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
