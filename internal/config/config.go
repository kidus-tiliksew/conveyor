// Package config loads Conveyor's immutable deployment settings and the
// mutable workspace document. Phase 4.7 deliberately keeps execution
// credentials out of both documents: conveyord uses CONVEYOR_API_KEY and MCP
// clients bring their own agent credentials (design-system-architecture; DEC-3).
package config

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
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
	Name     string `yaml:"name" json:"name"`
	URL      string `yaml:"url" json:"url"`
	GitHub   string `yaml:"github,omitempty" json:"github,omitempty"`
	Base     string `yaml:"base" json:"base"`
	Checkout string `yaml:"checkout,omitempty" json:"checkout,omitempty"`

	// Accepted only while canonicalizing pre-4.7 stored rows. These fields
	// are cleared by normalize and never cross the workspace API boundary.
	LegacyImage      string         `yaml:"image,omitempty" json:"-"`
	LegacySecretRefs []string       `yaml:"secret_refs,omitempty" json:"-"`
	LegacyToolPolicy map[string]any `yaml:"tool_policy,omitempty" json:"-"`
}

// MonitorConfig is explicit workspace/repository observation scope. It carries
// no credentials; the GitHub boundary uses the daemon's least-privilege
// environment and records only stable error categories (design-monitor-drift).
type MonitorConfig struct {
	Enabled           bool          `yaml:"enabled" json:"enabled"`
	Repositories      []string      `yaml:"repositories,omitempty" json:"repositories"`
	PollInterval      time.Duration `yaml:"-" json:"-"`
	PollIntervalText  string        `yaml:"poll_interval" json:"poll_interval"`
	StartupWindow     time.Duration `yaml:"-" json:"-"`
	StartupWindowText string        `yaml:"startup_window" json:"startup_window"`
}

type Database struct {
	Backend string `yaml:"backend"`
	URL     string `yaml:"url"`
}

type ExecutionMode string

const (
	ExecutionInProcess ExecutionMode = "in_process"
	ExecutionMCP       ExecutionMode = "mcp"

	ControlPlaneModelEnv        = "CONVEYOR_CONTROL_PLANE_MODEL"
	TriageModelEnv              = "CONVEYOR_TRIAGE_MODEL"
	PlanningModelEnv            = "CONVEYOR_PLANNING_MODEL"
	OrganizationNameEnv         = "CONVEYOR_ORGANIZATION_NAME"
	FirstOperatorEmailEnv       = "CONVEYOR_FIRST_OPERATOR_EMAIL"
	FirstOperatorDisplayNameEnv = "CONVEYOR_FIRST_OPERATOR_DISPLAY_NAME"
	SMTPHostEnv                 = "CONVEYOR_SMTP_HOST"
	SMTPPortEnv                 = "CONVEYOR_SMTP_PORT"
	SMTPUsernameEnv             = "CONVEYOR_SMTP_USERNAME"
	SMTPPasswordEnv             = "CONVEYOR_SMTP_PASSWORD"
	SMTPFromEnv                 = "CONVEYOR_SMTP_FROM"
	PublicURLEnv                = "CONVEYOR_PUBLIC_URL"
)

// InvitationDelivery is process-only configuration. In particular, the SMTP
// password can never enter YAML, the workspace API, or an event payload.
type InvitationDelivery struct {
	Host, Port, Username, Password, From, PublicURL string
	// TLSConfig is an in-memory test/deployment trust override. Environment
	// loading leaves it nil so system roots and TLS 1.2 remain the default.
	TLSConfig *tls.Config `yaml:"-" json:"-"`
}

func InvitationDeliveryFromEnvironment() InvitationDelivery {
	c := InvitationDelivery{
		Host: strings.TrimSpace(os.Getenv(SMTPHostEnv)), Port: strings.TrimSpace(os.Getenv(SMTPPortEnv)),
		Username: strings.TrimSpace(os.Getenv(SMTPUsernameEnv)), Password: os.Getenv(SMTPPasswordEnv),
		From: strings.TrimSpace(os.Getenv(SMTPFromEnv)), PublicURL: strings.TrimRight(strings.TrimSpace(os.Getenv(PublicURLEnv)), "/"),
	}
	if c.Port == "" {
		c.Port = "587"
	}
	return c
}

func (c InvitationDelivery) SMTPConfigured() bool { return c.Host != "" && c.From != "" }

// FirstOperatorIdentity is process-only bootstrap input. It is deliberately
// absent from Config and WorkspaceDocument so identity never enters persisted
// deployment or workspace configuration.
type FirstOperatorIdentity struct {
	OrganizationName string
	Email            string
	DisplayName      string
}

func FirstOperatorIdentityFromEnvironment() FirstOperatorIdentity {
	identity := FirstOperatorIdentity{
		OrganizationName: "Conveyor",
		Email:            "operator@localhost",
		DisplayName:      "Local Operator",
	}
	if value := strings.TrimSpace(os.Getenv(OrganizationNameEnv)); value != "" {
		identity.OrganizationName = value
	}
	if value := strings.TrimSpace(os.Getenv(FirstOperatorEmailEnv)); value != "" {
		identity.Email = strings.ToLower(value)
	}
	if value := strings.TrimSpace(os.Getenv(FirstOperatorDisplayNameEnv)); value != "" {
		identity.DisplayName = value
	}
	return identity
}

// ModelEnvironmentOverride is one non-persistent deployment override for an
// in-process control-plane model. It is deliberately separate from Config so
// workspace documents and their projections continue to describe stored state.
type ModelEnvironmentOverride struct {
	Variable string
	Model    string
}

// ActiveControlPlaneModelOverrides returns every non-empty process-level model
// override for startup observability. Whitespace-only values are unset.
func ActiveControlPlaneModelOverrides() []ModelEnvironmentOverride {
	overrides := make([]ModelEnvironmentOverride, 0, 3)
	for _, variable := range []string{ControlPlaneModelEnv, TriageModelEnv, PlanningModelEnv} {
		if model := strings.TrimSpace(os.Getenv(variable)); model != "" {
			overrides = append(overrides, ModelEnvironmentOverride{Variable: variable, Model: model})
		}
	}
	return overrides
}

// ControlPlaneModelOverride resolves stage-specific > general environment
// precedence without consulting or changing the stored workspace config.
func ControlPlaneModelOverride(stage string) (string, bool) {
	model := strings.TrimSpace(os.Getenv(ControlPlaneModelEnv))
	variable := ""
	switch strings.TrimSpace(stage) {
	case "triage":
		variable = TriageModelEnv
	case "planning":
		variable = PlanningModelEnv
	}
	if stageModel := strings.TrimSpace(os.Getenv(variable)); stageModel != "" {
		model = stageModel
	}
	return model, model != ""
}

// ResolveControlPlaneModel returns the process-effective model for an
// in-process invocation while preserving the supplied stored value unchanged.
func ResolveControlPlaneModel(stage, stored string) string {
	if model, ok := ControlPlaneModelOverride(stage); ok {
		return model
	}
	return stored
}

type StageRoute struct {
	Model       string        `yaml:"model" json:"model"`
	ModelPolicy string        `yaml:"model_policy,omitempty" json:"model_policy,omitempty"`
	Harness     string        `yaml:"harness,omitempty" json:"harness,omitempty"`
	Effort      string        `yaml:"effort,omitempty" json:"effort,omitempty"`
	Timeout     time.Duration `yaml:"-" json:"-"`
	TimeoutText string        `yaml:"timeout" json:"timeout"`
	Execution   ExecutionMode `yaml:"execution" json:"execution"`
	// EffectiveModel is the normalized worker argument. It is deliberately
	// absent from persisted compatibility routes (design-harness-execution).
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
	StallTimeout          time.Duration       `yaml:"-" json:"-"`
	StallTimeoutText      string              `yaml:"stall_timeout,omitempty" json:"stall_timeout,omitempty"`
}

const (
	// MCP transport controls the representation substituted for the whole
	// {mcp_config} argv element (design-harness-execution).
	MCPTransportJSONFile     = "json_file"
	MCPTransportTOMLOverride = "toml_override"
	MCPTransportEnvironment  = "environment"
)

var mcpAttachmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ReviewSeat is one immutable assignment in a submitted review round. The
// model is always pinned; Harness optionally overrides the workspace review
// route for worker dispatch (design-harness-execution).
type ReviewSeat struct {
	Model   string `yaml:"model" json:"model"`
	Harness string `yaml:"harness,omitempty" json:"harness,omitempty"`
	Effort  string `yaml:"effort,omitempty" json:"effort,omitempty"`
}

type ReviewPanel struct {
	Seats []ReviewSeat `yaml:"seats,omitempty" json:"seats"`
}

type ExecutionPolicy struct {
	// DefaultMode is deprecated (DEC-5): parsed from legacy documents
	// and seeds for compatibility, never read for behavior, dropped on save.
	DefaultMode          string `yaml:"default_mode,omitempty" json:"default_mode,omitempty"`
	SpecApproval         bool   `yaml:"spec_approval" json:"spec_approval"`
	MergeApproval        bool   `yaml:"merge_approval" json:"merge_approval"`
	ImplementConcurrency int    `yaml:"implement_concurrency" json:"implement_concurrency"`
	ReviewConcurrency    int    `yaml:"review_concurrency" json:"review_concurrency"`
	// RequireVerificationEvidence fails review submission closed until the
	// task owns an eligible screenshot or short recording.
	RequireVerificationEvidence bool `yaml:"require_verification_evidence" json:"require_verification_evidence"`
	// FirstActivityTimeout is worker child-output liveness, independent of
	// the claim lease and fixed execution deadline (design-260805-973cd4).
	FirstActivityTimeout     time.Duration `yaml:"-" json:"-"`
	FirstActivityTimeoutText string        `yaml:"first_activity_timeout" json:"first_activity_timeout"`
}

const (
	DefaultStageTimeout              = 2 * time.Hour
	DefaultFirstActivityTimeout      = 2 * time.Minute
	DefaultFirstActivityTimeoutText  = "2m"
	DefaultHarnessStallTimeout       = 10 * time.Minute
	DefaultHarnessStallTimeoutText   = "10m"
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
	Effort      string `yaml:"effort,omitempty" json:"effort,omitempty"`
	TimeoutText string `yaml:"timeout" json:"timeout"`
}

const (
	DefaultPlanningExplorationOutputTokens = 10_000
	DefaultServedRequirementAuthorityNodes = 256
	MinServedRequirementAuthorityNodes     = 8
	// Depth is a reachability ceiling; the independent node cap remains the
	// conservative fan-out bound and may truncate wide graphs before depth five.
	DefaultLineageContextDepth           = 5
	DefaultLineageContextNodes           = 32
	DefaultLineageContextRenderableBytes = 256 << 10
	DefaultLineageContextArtifactRefs    = 64
	DefaultLineageContextLinksPerNode    = 4
)

// LineageContextSettings bounds the shared graph context assembled for both
// planning and delivery agents. Values are snapshotted when context is built;
// a workspace hot reload therefore cannot mutate an already returned payload.
type LineageContextSettings struct {
	Depth           int `yaml:"depth" json:"depth"`
	Nodes           int `yaml:"nodes" json:"nodes"`
	RenderableBytes int `yaml:"renderable_bytes" json:"renderable_bytes"`
	ArtifactRefs    int `yaml:"artifact_refs" json:"artifact_refs"`
	AuthorityNodes  int `yaml:"authority_nodes" json:"authority_nodes"`
}

type PlanningSettings struct {
	Model                   string                 `yaml:"model" json:"model"`
	Effort                  string                 `yaml:"effort,omitempty" json:"effort,omitempty"`
	TimeoutText             string                 `yaml:"timeout" json:"timeout"`
	ExplorationOutputTokens int                    `yaml:"exploration_output_tokens" json:"exploration_output_tokens"`
	Context                 LineageContextSettings `yaml:"context" json:"context"`
}

type ControlPlaneSettings struct {
	Triage   ModelTimeoutSettings `yaml:"triage" json:"triage"`
	Planning PlanningSettings     `yaml:"planning" json:"planning"`
	// Spec accepts pre-v1.34 documents. Normalization moves it into the
	// contextual spec execution settings and never emits it again (design-harness-execution).
	Spec ModelTimeoutSettings `yaml:"spec,omitempty" json:"spec,omitempty"`
}

// MarshalJSON keeps the canonical control-plane surface while the legacy spec
// field above remains readable for stored pre-v1.34 documents.
func (c ControlPlaneSettings) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Triage   ModelTimeoutSettings `json:"triage"`
		Planning PlanningSettings     `json:"planning"`
	}{Triage: c.Triage, Planning: c.Planning})
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
// object is present (design-harness-execution).
type ContextualExecutionSettings struct {
	ControlPlane   ControlPlaneSettings    `yaml:"control_plane" json:"control_plane"`
	Spec           ImplementationSettings  `yaml:"spec" json:"spec"`
	Implementation ImplementationSettings  `yaml:"implementation" json:"implementation"`
	Review         ReviewExecutionSettings `yaml:"review" json:"review"`
}

// ExecutionSetup is one named execution contract. Harness definitions remain
// workspace-scoped; the settings and review panel are frozen onto a task at
// intake (design-harness-execution; DEC-7).
type ExecutionSetup struct {
	Name              string                      `yaml:"name" json:"name"`
	ExecutionSettings ContextualExecutionSettings `yaml:"execution_settings" json:"execution_settings"`
	Review            ReviewPanel                 `yaml:"review" json:"review"`
	RefreshReview     string                      `yaml:"refresh_review,omitempty" json:"refresh_review"`
}

const (
	RefreshReviewDelta = "delta"
	RefreshReviewFull  = "full"
	RefreshReviewNone  = "none"
)

type WorkspaceDocument struct {
	Workspace                 string                       `yaml:"workspace" json:"workspace"`
	MaxBounces                int                          `yaml:"max_bounces" json:"max_bounces"`
	WorkOrderQueueTimeoutText string                       `yaml:"work_order_queue_timeout" json:"work_order_queue_timeout"`
	ExecutionSettings         *ContextualExecutionSettings `yaml:"execution_settings,omitempty" json:"execution_settings,omitempty"`
	Routing                   Routing                      `yaml:"routing" json:"routing"`
	Harnesses                 []Harness                    `yaml:"harnesses,omitempty" json:"harnesses"`
	Review                    ReviewPanel                  `yaml:"review" json:"review"`
	Setups                    []ExecutionSetup             `yaml:"setups,omitempty" json:"setups"`
	DefaultSetup              string                       `yaml:"default_setup,omitempty" json:"default_setup"`
	Execution                 ExecutionPolicy              `yaml:"execution" json:"execution"`
	Repos                     []Repo                       `yaml:"repos" json:"repos"`
	Monitor                   MonitorConfig                `yaml:"monitor" json:"monitor"`
	PlanningModels            []string                     `yaml:"planning_models,omitempty" json:"planning_models"`

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
	Setups                    []ExecutionSetup             `yaml:"setups,omitempty"`
	DefaultSetup              string                       `yaml:"default_setup,omitempty"`
	Execution                 ExecutionPolicy              `yaml:"execution"`
	Repos                     []Repo                       `yaml:"repos"`
	Monitor                   MonitorConfig                `yaml:"monitor"`
	PlanningModels            []string                     `yaml:"planning_models,omitempty"`
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
	next.Setups = document.Setups
	next.DefaultSetup = document.DefaultSetup
	next.Execution = document.Execution
	next.Repos = document.Repos
	next.Monitor = document.Monitor
	next.PlanningModels = document.PlanningModels
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
	legacy := hadBudget || document.LegacyImage != "" || document.WorkOrderQueueTimeoutText == "" ||
		document.Execution.FirstActivityTimeoutText == "" || document.Review.Seats == nil ||
		document.ExecutionSettings == nil || document.Setups == nil
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
// boot once and be rewritten in the v1.6 shape. New deployment
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
	legacy := settings.ControlPlane.Spec
	if settings.Spec.Model == "" && legacy.Model != "" {
		settings.Spec.Model = legacy.Model
		// The implementation harness is the unambiguous legacy worker context;
		// otherwise a single registered harness is safe to adopt.
		if settings.Spec.Harness == "" {
			settings.Spec.Harness = settings.Implementation.Harness
		}
		if settings.Spec.Harness == "" && len(c.Harnesses) == 1 {
			settings.Spec.Harness = c.Harnesses[0].Name
		}
		if settings.Spec.ModelPolicy == "" {
			settings.Spec.ModelPolicy = ModelPolicyExplicit
		}
	}
	if settings.Spec.TimeoutText == "" && legacy.TimeoutText != "" {
		settings.Spec.TimeoutText = legacy.TimeoutText
	}
	settings.ControlPlane.Spec = ModelTimeoutSettings{}
	if settings.ControlPlane.Planning.Model == "" {
		settings.ControlPlane.Planning.Model = settings.ControlPlane.Triage.Model
	}
	if settings.ControlPlane.Planning.Effort == "" {
		settings.ControlPlane.Planning.Effort = settings.ControlPlane.Triage.Effort
	}
	if settings.ControlPlane.Planning.TimeoutText == "" {
		settings.ControlPlane.Planning.TimeoutText = settings.ControlPlane.Triage.TimeoutText
	}
	if settings.ControlPlane.Planning.ExplorationOutputTokens == 0 {
		settings.ControlPlane.Planning.ExplorationOutputTokens = DefaultPlanningExplorationOutputTokens
	}
	defaultLineageContextSettings(&settings.ControlPlane.Planning.Context)
	if c.Routing.Stages == nil {
		c.Routing.Stages = map[string]StageRoute{}
	}
	c.Routing.Stages["triage"] = StageRoute{
		Model: settings.ControlPlane.Triage.Model, Effort: settings.ControlPlane.Triage.Effort,
		TimeoutText: settings.ControlPlane.Triage.TimeoutText,
		Execution:   ExecutionInProcess, ModelPolicy: ModelPolicyExplicit,
	}
	c.Routing.Stages["spec"] = StageRoute{
		Model: settings.Spec.Model, Harness: settings.Spec.Harness, Effort: settings.Spec.Effort,
		TimeoutText: settings.Spec.TimeoutText,
		Execution:   ExecutionMCP, ModelPolicy: settings.Spec.ModelPolicy,
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
	specHadModel := spec.Model != ""
	if spec.Harness == "" {
		spec.Harness = implement.Harness
	}
	if spec.Model == "" {
		spec.Model = implement.Model
	}
	if spec.ModelPolicy == "" {
		if specHadModel {
			spec.ModelPolicy = ModelPolicyExplicit
		} else {
			spec.ModelPolicy = implement.ModelPolicy
		}
	}
	if spec.TimeoutText == "" {
		spec.TimeoutText = implement.TimeoutText
	}
	review := routing.Stages["review"]
	return &ContextualExecutionSettings{
		ControlPlane: ControlPlaneSettings{
			Triage: ModelTimeoutSettings{Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText},
			Planning: PlanningSettings{
				Model: triage.Model, Effort: triage.Effort, TimeoutText: triage.TimeoutText,
				ExplorationOutputTokens: DefaultPlanningExplorationOutputTokens,
			},
		},
		Spec: ImplementationSettings{Harness: spec.Harness, Model: spec.Model, ModelPolicy: spec.ModelPolicy, Effort: spec.Effort, TimeoutText: spec.TimeoutText},
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

func stageName(stage string) string {
	if stage == "implement" {
		return "implementation"
	}
	return stage
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
		// is usable (design-harness-execution).
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
	setups := append([]ExecutionSetup(nil), c.Setups...)
	defaultSetup := strings.TrimSpace(c.DefaultSetup)
	if c.Setups == nil {
		legacy := *c
		legacy.Setups = nil
		legacy.DefaultSetup = ""
		normalized, err := normalizeLegacy(&legacy, path)
		if err != nil {
			return nil, err
		}
		setups = []ExecutionSetup{{Name: "default", ExecutionSettings: *normalized.ExecutionSettings, Review: normalized.Review, RefreshReview: RefreshReviewDelta}}
		defaultSetup = "default"
	} else if len(setups) == 0 {
		return nil, fmt.Errorf("setups must contain at least one setup")
	}
	if defaultSetup == "" {
		return nil, fmt.Errorf("default_setup is required")
	}

	names := make(map[string]struct{}, len(setups))
	var defaultConfig *Config
	normalizedSetups := make([]ExecutionSetup, 0, len(setups))
	for i, setup := range setups {
		setup.Name = strings.TrimSpace(setup.Name)
		if setup.Name == "" || !safePathSegment(setup.Name) {
			return nil, fmt.Errorf("setups[%d].name must be one path-safe segment", i)
		}
		if _, duplicate := names[setup.Name]; duplicate {
			return nil, fmt.Errorf("duplicate setup name %q", setup.Name)
		}
		names[setup.Name] = struct{}{}
		setup.RefreshReview = strings.TrimSpace(setup.RefreshReview)
		if setup.RefreshReview == "" {
			setup.RefreshReview = RefreshReviewDelta
		}
		if setup.RefreshReview != RefreshReviewDelta && setup.RefreshReview != RefreshReviewFull && setup.RefreshReview != RefreshReviewNone {
			return nil, fmt.Errorf("setup %q: refresh_review must be delta, full, or none", setup.Name)
		}
		candidate := *c
		candidate.Setups = nil
		candidate.DefaultSetup = ""
		candidate.ExecutionSettings = &setup.ExecutionSettings
		candidate.Review = setup.Review
		candidate.Routing = Routing{Stages: map[string]StageRoute{}}
		normalized, err := normalizeLegacy(&candidate, path)
		if err != nil {
			return nil, fmt.Errorf("setup %q: %w", setup.Name, err)
		}
		normalizedSetup := ExecutionSetup{Name: setup.Name, ExecutionSettings: *normalized.ExecutionSettings, Review: normalized.Review, RefreshReview: setup.RefreshReview}
		normalizedSetups = append(normalizedSetups, normalizedSetup)
		if setup.Name == defaultSetup {
			defaultConfig = normalized
		}
	}
	if defaultConfig == nil {
		return nil, fmt.Errorf("default_setup %q does not name a configured setup", defaultSetup)
	}
	result := *defaultConfig
	result.Setups = normalizedSetups
	result.DefaultSetup = defaultSetup
	// The legacy singleton is a read-only projection of the default setup.
	for i := range normalizedSetups {
		if normalizedSetups[i].Name == defaultSetup {
			result.ExecutionSettings = &normalizedSetups[i].ExecutionSettings
			result.Review = normalizedSetups[i].Review
			break
		}
	}
	return &result, nil
}

func normalizeLegacy(c *Config, path string) (*Config, error) {
	var requestedPlanning PlanningSettings
	if c.ExecutionSettings != nil {
		requestedPlanning = c.ExecutionSettings.ControlPlane.Planning
	}
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
	if c.Monitor.PollIntervalText == "" {
		c.Monitor.PollIntervalText = "1m"
	}
	monitorPoll, err := time.ParseDuration(c.Monitor.PollIntervalText)
	if err != nil || monitorPoll <= 0 {
		return nil, fmt.Errorf("monitor.poll_interval must be a positive duration")
	}
	c.Monitor.PollInterval = monitorPoll
	if c.Monitor.StartupWindowText == "" {
		c.Monitor.StartupWindowText = "24h"
	}
	startupWindow, err := time.ParseDuration(c.Monitor.StartupWindowText)
	if err != nil || startupWindow <= 0 {
		return nil, fmt.Errorf("monitor.startup_window must be a positive duration")
	}
	c.Monitor.StartupWindow = startupWindow
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
	// An absent execution block means the shipped default: both gates on
	// (§21.12 change 2; the mode axis itself is removed by §21.31).
	executionDefaultsProbe := c.Execution
	executionDefaultsProbe.RequireVerificationEvidence = false
	if executionDefaultsProbe == (ExecutionPolicy{}) {
		c.Execution.SpecApproval = true
		c.Execution.MergeApproval = true
	}
	if c.Execution.DefaultMode != "" && c.Execution.DefaultMode != "auto" && c.Execution.DefaultMode != "manual" {
		return nil, fmt.Errorf("execution.default_mode is deprecated (DEC-5) and must be auto or manual when present")
	}
	// Legacy documents keep their stored value readable, but it is never
	// re-emitted or consulted; normalization drops it.
	c.Execution.DefaultMode = ""
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
	if c.Execution.FirstActivityTimeoutText == "" {
		c.Execution.FirstActivityTimeout = DefaultFirstActivityTimeout
		c.Execution.FirstActivityTimeoutText = DefaultFirstActivityTimeoutText
	} else {
		parsed, parseErr := time.ParseDuration(c.Execution.FirstActivityTimeoutText)
		if parseErr != nil || parsed <= 0 {
			return nil, fmt.Errorf("execution.first_activity_timeout must be a positive duration")
		}
		c.Execution.FirstActivityTimeout = parsed
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
		if harness.StallTimeoutText == "" {
			harness.StallTimeoutText = DefaultHarnessStallTimeoutText
		}
		if err := validateHarness(*harness, i); err != nil {
			return nil, err
		}
		parsed, _ := time.ParseDuration(harness.ProbeTimeoutText)
		harness.ProbeTimeout = parsed
		harness.StallTimeout, _ = time.ParseDuration(harness.StallTimeoutText)
		harnesses[harness.Name] = *harness
	}
	for stage, route := range c.Routing.Stages {
		if stage == "spec" {
			// Pre-v1.34 stored routes were fixed in-process. Upgrade them to the
			// worker context without allowing legacy values to override explicit
			// contextual spec settings (design-harness-execution).
			if route.Execution == "" || route.Execution == ExecutionInProcess {
				route.Execution = ExecutionMCP
			}
			if route.Harness == "" {
				route.Harness = c.Routing.Stages["implement"].Harness
			}
		}
		route.Effort = strings.TrimSpace(route.Effort)
		if stage != "triage" && stage != "spec" && stage != "implement" && route.Effort != "" {
			return nil, fmt.Errorf("routing stage %s: effort is not supported", stage)
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
			case "triage":
				route.Execution = ExecutionInProcess
			default:
				route.Execution = ExecutionMCP
			}
		}
		if route.Execution != ExecutionInProcess && route.Execution != ExecutionMCP {
			return nil, fmt.Errorf("routing stage %s: execution must be in_process or mcp", stage)
		}
		if stage == "triage" && route.Execution != ExecutionInProcess {
			return nil, fmt.Errorf("routing stage triage: execution is fixed to in_process")
		}
		if (stage == "spec" || stage == "implement") && route.Execution != ExecutionMCP {
			return nil, fmt.Errorf("routing stage %s: execution is fixed to mcp", stage)
		}
		if stage == "triage" {
			if route.Effort != "" && !validResponsesEffort(route.Effort) {
				return nil, fmt.Errorf("execution_settings.control_plane.%s.effort %q must be minimal, low, medium, or high", stage, route.Effort)
			}
			// Harness fields on pre-v1.18 control-plane routes are compatibility
			// noise and never become worker requirements (design-harness-execution).
			route.Harness = ""
		}
		if stage == "review" && route.Execution == ExecutionInProcess {
			if route.Harness != "" {
				return nil, fmt.Errorf("routing stage review: in_process execution cannot select a harness")
			}
		} else if stage == "spec" || stage == "implement" {
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
					return nil, fmt.Errorf("execution_settings.%s.effort must be low, medium, or high", stageName(stage))
				}
				if len(implementationHarness.EffortArgs[route.Effort]) == 0 {
					return nil, fmt.Errorf("execution_settings.%s.effort %q is not supported by harness %q", stageName(stage), route.Effort, route.Harness)
				}
			}
			effective, modelErr := normalizeHarnessModel(route, c.Harnesses)
			if modelErr != nil {
				return nil, fmt.Errorf("routing stage %s: %w", stage, modelErr)
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
		// explicit; it is retained only for compatibility (design-harness-execution).
		c.Routing.Stages["review"] = reviewRoute
	}
	for _, stage := range []string{"spec", "implement", "review"} {
		route := c.Routing.Stages[stage]
		if route.Execution == ExecutionMCP && c.Execution.FirstActivityTimeout >= route.Timeout {
			return nil, fmt.Errorf("execution.first_activity_timeout must be shorter than %s execution timeout", stageName(stage))
		}
	}
	normalizedSettings := contextualExecutionSettings(c.Routing)
	planning := requestedPlanning
	if planning.Model == "" {
		planning.Model = normalizedSettings.ControlPlane.Triage.Model
	}
	if planning.Effort == "" {
		planning.Effort = normalizedSettings.ControlPlane.Triage.Effort
	}
	if planning.TimeoutText == "" {
		planning.TimeoutText = normalizedSettings.ControlPlane.Triage.TimeoutText
	}
	if planning.ExplorationOutputTokens == 0 {
		planning.ExplorationOutputTokens = DefaultPlanningExplorationOutputTokens
	}
	defaultLineageContextSettings(&planning.Context)
	planning.Model = strings.TrimSpace(planning.Model)
	planning.Effort = strings.TrimSpace(planning.Effort)
	if planning.Model == "" {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.model is required")
	}
	if planning.Effort != "" && !validResponsesEffort(planning.Effort) {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.effort %q must be minimal, low, medium, or high", planning.Effort)
	}
	if parsed, parseErr := time.ParseDuration(planning.TimeoutText); parseErr != nil || parsed <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.timeout must be a positive duration")
	}
	if planning.ExplorationOutputTokens <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.exploration_output_tokens must be positive")
	}
	if planning.Context.Depth <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.context.depth must be positive")
	}
	if planning.Context.Nodes <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.context.nodes must be positive")
	}
	if planning.Context.RenderableBytes <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.context.renderable_bytes must be positive")
	}
	if planning.Context.ArtifactRefs <= 0 {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.context.artifact_refs must be positive")
	}
	if planning.Context.AuthorityNodes < MinServedRequirementAuthorityNodes {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.context.authority_nodes must be at least %d", MinServedRequirementAuthorityNodes)
	}
	normalizedSettings.ControlPlane.Planning = planning
	c.ExecutionSettings = normalizedSettings
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
		// Repository names become part of the implicit sibling worktree name,
		// so keep them to the server-generated task ID alphabet (design-git-delivery).
		if !validRepoName(repo.Name) {
			return nil, fmt.Errorf("repo %d: name %q must use only ASCII letters, digits, '.', '_', or '-' and must not be '.' or '..'", i, repo.Name)
		}
		if repo.Base == "" {
			repo.Base = "main"
		}
		if repo.Checkout != "" {
			if !filepath.IsAbs(repo.Checkout) {
				return nil, fmt.Errorf("repo %d: checkout must be an absolute path", i)
			}
			repo.Checkout = filepath.Clean(repo.Checkout)
		}
		repo.LegacyImage = ""
		repo.LegacySecretRefs = nil
		repo.LegacyToolPolicy = nil
	}
	if len(c.PlanningModels) == 0 {
		c.PlanningModels = []string{planning.Model}
	}
	seenPlanningModels := make(map[string]struct{}, len(c.PlanningModels))
	defaultAllowed := false
	for i, model := range c.PlanningModels {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, fmt.Errorf("planning_models[%d] is required", i)
		}
		if _, duplicate := seenPlanningModels[model]; duplicate {
			return nil, fmt.Errorf("planning_models contains duplicate model %q", model)
		}
		seenPlanningModels[model] = struct{}{}
		defaultAllowed = defaultAllowed || model == planning.Model
		c.PlanningModels[i] = model
	}
	if !defaultAllowed {
		return nil, fmt.Errorf("execution_settings.control_plane.planning.model %q must be included in planning_models", planning.Model)
	}
	seenMonitorRepo := make(map[string]struct{}, len(c.Monitor.Repositories))
	for i, name := range c.Monitor.Repositories {
		name = strings.TrimSpace(name)
		if _, ok := repoNames[name]; !ok {
			return nil, fmt.Errorf("monitor.repositories[%d] names unknown repo %q", i, name)
		}
		if _, duplicate := seenMonitorRepo[name]; duplicate {
			return nil, fmt.Errorf("monitor.repositories contains duplicate repo %q", name)
		}
		seenMonitorRepo[name] = struct{}{}
		c.Monitor.Repositories[i] = name
	}
	if c.Monitor.Enabled && len(c.Monitor.Repositories) == 0 {
		return nil, fmt.Errorf("monitor.repositories is required when monitor is enabled")
	}
	return c, nil
}

func defaultLineageContextSettings(settings *LineageContextSettings) {
	if settings.Depth == 0 {
		settings.Depth = DefaultLineageContextDepth
	}
	if settings.Nodes == 0 {
		settings.Nodes = DefaultLineageContextNodes
	}
	if settings.RenderableBytes == 0 {
		settings.RenderableBytes = DefaultLineageContextRenderableBytes
	}
	if settings.ArtifactRefs == 0 {
		settings.ArtifactRefs = DefaultLineageContextArtifactRefs
	}
	if settings.AuthorityNodes == 0 {
		settings.AuthorityNodes = DefaultServedRequirementAuthorityNodes
	}
}

func ServedRequirementAuthorityNodes(cfg *Config) int {
	if cfg != nil && cfg.ExecutionSettings != nil {
		if limit := cfg.ExecutionSettings.ControlPlane.Planning.Context.AuthorityNodes; limit > 0 {
			return limit
		}
	}
	return DefaultServedRequirementAuthorityNodes
}

func (c *Config) WorkspaceDocument() WorkspaceDocument {
	reviewSeats := append(make([]ReviewSeat, 0, len(c.Review.Seats)), c.Review.Seats...)
	if len(reviewSeats) == 0 {
		if route, ok := c.Routing.Stages["review"]; ok && route.Model != "" {
			reviewSeats = append(reviewSeats, ReviewSeat{Model: route.Model, Harness: route.Harness})
		}
	}
	execution := c.Execution
	if execution.FirstActivityTimeoutText == "" {
		execution.FirstActivityTimeout = DefaultFirstActivityTimeout
		execution.FirstActivityTimeoutText = DefaultFirstActivityTimeoutText
	}
	executionSettings := contextualExecutionSettings(c.Routing)
	if c.ExecutionSettings != nil {
		executionSettings.ControlPlane.Planning = c.ExecutionSettings.ControlPlane.Planning
	}
	document := WorkspaceDocument{
		Workspace: c.Workspace, MaxBounces: c.MaxBounces,
		WorkOrderQueueTimeoutText: c.WorkOrderQueueTimeoutText,
		ExecutionSettings:         executionSettings,
		Routing:                   Routing{Stages: make(map[string]StageRoute, len(c.Routing.Stages))},
		Harnesses:                 append(make([]Harness, 0, len(c.Harnesses)), c.Harnesses...),
		Review:                    ReviewPanel{Seats: reviewSeats},
		Setups:                    append([]ExecutionSetup(nil), c.Setups...),
		DefaultSetup:              c.DefaultSetup,
		Execution:                 execution,
		Repos:                     append(make([]Repo, 0, len(c.Repos)), c.Repos...),
		Monitor:                   c.Monitor,
		PlanningModels:            append([]string(nil), c.PlanningModels...),
	}
	document.Monitor.PollInterval = 0
	document.Monitor.StartupWindow = 0
	document.Monitor.Repositories = append(make([]string, 0, len(c.Monitor.Repositories)), c.Monitor.Repositories...)
	for stage, route := range c.Routing.Stages {
		route.Timeout = 0
		route.EffectiveModel = ""
		route.LegacyHarnesses = nil
		route.LegacyModelTier = ""
		document.Routing.Stages[stage] = route
	}
	return document
}

// Setup resolves a configured setup by name, defaulting an empty selector to
// the workspace default. Unknown explicit names never fall back (design-harness-execution).
func (c *Config) Setup(name string) (ExecutionSetup, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = c.DefaultSetup
		if name == "" && len(c.Setups) == 0 {
			name = "default"
		}
	}
	for _, setup := range c.Setups {
		if setup.Name == name {
			return setup, true
		}
	}
	if len(c.Setups) == 0 && name == "default" {
		settings := c.ExecutionSettings
		if settings == nil && len(c.Routing.Stages) != 0 {
			settings = contextualExecutionSettings(c.Routing)
		}
		if settings != nil {
			return ExecutionSetup{Name: "default", ExecutionSettings: *settings, Review: c.Review, RefreshReview: RefreshReviewDelta}, true
		}
	}
	return ExecutionSetup{}, false
}

// WithSetup returns the existing normalized config projected through one
// already-normalized setup. It is used for task-frozen dispatch and health
// calculations without consulting the mutable setup name again.
func (c *Config) WithSetup(setup ExecutionSetup) *Config {
	next := *c
	next.Setups = []ExecutionSetup{setup}
	next.DefaultSetup = setup.Name
	next.ExecutionSettings = &next.Setups[0].ExecutionSettings
	next.Review = setup.Review
	next.Routing = Routing{Stages: make(map[string]StageRoute, 4)}
	applyContextualExecutionSettings(&next)
	for stage, route := range next.Routing.Stages {
		route.Timeout, _ = time.ParseDuration(route.TimeoutText)
		if stage == "implement" {
			route.EffectiveModel, _ = normalizeHarnessModel(route, next.Harnesses)
		}
		next.Routing.Stages[stage] = route
	}
	return &next
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
	if h.StallTimeoutText != "" {
		parsed, parseErr = time.ParseDuration(h.StallTimeoutText)
		if parseErr != nil || parsed < 0 || parsed == 0 && strings.TrimSpace(h.StallTimeoutText) != "0" {
			return fmt.Errorf("harnesses[%d].stall_timeout must be 0 to disable or a positive duration", index)
		}
	}
	return nil
}

// ValidateHarness applies the same transport-aware durable contract to worker
// snapshots immediately before probing or launch (design-harness-execution).
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

func validResponsesEffort(effort string) bool {
	return effort == "minimal" || validEffort(effort)
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

func validRepoName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
