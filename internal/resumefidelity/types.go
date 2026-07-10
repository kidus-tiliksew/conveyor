// Package resumefidelity runs the Codex continuity experiment required by
// spec §20.2. It deliberately talks to the CLI through Docker rather than the
// adapter: the experiment compares CLI versions and failure boundaries that a
// pinned production adapter must normally reject.
package resumefidelity

import "time"

const (
	DefaultBaseImage   = "conveyor-base:dev"
	DefaultBumpImage   = "conveyor-base:codex-0.143.0"
	DefaultBaseVersion = "0.142.0"
	DefaultBumpVersion = "0.143.0"
	continuityMarker   = "amber-orchid-27"
)

// Config controls one live experiment run.
type Config struct {
	BaseImage      string
	BumpImage      string
	BaseVersion    string
	BumpVersion    string
	AuthPath       string
	OutputDir      string
	WorkRoot       string
	KeepWork       bool
	CommandTimeout time.Duration
	CrashTimeout   time.Duration
}

// Result is the machine-readable experiment artifact.
type Result struct {
	SchemaVersion    int              `json:"schema_version"`
	RunID            string           `json:"run_id"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       time.Time        `json:"finished_at"`
	BaseImage        string           `json:"base_image"`
	BaseVersion      string           `json:"base_version"`
	BumpImage        string           `json:"bump_image"`
	BumpVersion      string           `json:"bump_version"`
	HostBoundary     string           `json:"host_boundary"`
	Scoring          ScoringPolicy    `json:"scoring"`
	Scenarios        []ScenarioResult `json:"scenarios"`
	RoutingDefault   RoutingDefault   `json:"routing_default"`
	JSONArtifact     string           `json:"-"`
	MarkdownArtifact string           `json:"-"`
}

// ScoringPolicy makes the pass/fail calibration reproducible.
type ScoringPolicy struct {
	CoreMaximum            int     `json:"core_maximum"`
	ExtendedMaximum        int     `json:"extended_maximum"`
	ResumeCostCeilingRatio float64 `json:"resume_cost_ceiling_ratio"`
	EffectiveTokenFormula  string  `json:"effective_token_formula"`
}

// ScenarioResult compares native resume plus the handoff snapshot with a
// snapshot-briefed cold start at one failure boundary.
type ScenarioResult struct {
	Name            string      `json:"name"`
	Description     string      `json:"description"`
	SeedVersion     string      `json:"seed_version"`
	ProbeVersion    string      `json:"probe_version"`
	ContainerReused bool        `json:"container_reused"`
	SessionRestored bool        `json:"session_restored"`
	CrashObserved   bool        `json:"crash_observed"`
	SessionRef      string      `json:"session_ref,omitempty"`
	SeedEvents      string      `json:"seed_events"`
	CrashEvents     string      `json:"crash_events"`
	Resume          ProbeResult `json:"resume_plus_snapshot"`
	Cold            ProbeResult `json:"snapshot_cold_start"`
	Comparison      Comparison  `json:"comparison"`
	Recommendation  string      `json:"recommendation"`
	Error           string      `json:"error,omitempty"`
}

// ProbeResult captures one continuation's answer, score and CLI token usage.
type ProbeResult struct {
	Mode       string        `json:"mode"`
	Answer     ProbeAnswer   `json:"answer"`
	Score      RecallScore   `json:"score"`
	Usage      TokenUsage    `json:"usage"`
	Duration   time.Duration `json:"duration_ns"`
	Events     string        `json:"events"`
	CLIError   string        `json:"cli_error,omitempty"`
	ParseError string        `json:"parse_error,omitempty"`
}

// ProbeAnswer is constrained by probe-schema.json during each live run.
type ProbeAnswer struct {
	Decision            string   `json:"decision"`
	Rationale           []string `json:"rationale"`
	RejectedAlternative string   `json:"rejected_alternative"`
	ContinuityMarker    string   `json:"continuity_marker"`
}

// RecallScore awards four rationale points that both paths can recover from
// the snapshot, plus one native-context discriminator omitted from it.
type RecallScore struct {
	Decision            bool `json:"decision"`
	StaleWorkerFencing  bool `json:"stale_worker_fencing"`
	DeterministicReplay bool `json:"deterministic_replay"`
	RejectedHeartbeat   bool `json:"rejected_heartbeat_only"`
	NativeMarker        bool `json:"native_marker"`
	Core                int  `json:"core"`
	Extended            int  `json:"extended"`
}

// TokenUsage is read from Codex's turn.completed JSONL events. Effective is
// deliberately a token proxy, not a dollar claim for subscription-auth runs.
type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	EffectiveTokens       int64 `json:"uncached_input_plus_output_tokens"`
}

// Comparison records the cost and recall delta used by the routing rule.
type Comparison struct {
	CoreScoreDelta      int     `json:"core_score_delta"`
	ExtendedScoreDelta  int     `json:"extended_score_delta"`
	EffectiveTokenDelta int64   `json:"effective_token_delta"`
	ResumeToColdRatio   float64 `json:"resume_to_cold_ratio"`
	ResumeQualified     bool    `json:"resume_qualified"`
}

// RoutingDefault is the experiment's per-harness calibration output.
type RoutingDefault struct {
	Harness          string `json:"harness"`
	MatchingVersion  string `json:"matching_version"`
	CrossVersion     string `json:"cross_version"`
	SnapshotFallback string `json:"snapshot_fallback"`
	Rationale        string `json:"rationale"`
}
