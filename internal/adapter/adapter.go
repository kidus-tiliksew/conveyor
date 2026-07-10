// Package adapter defines the harness adapter interface (spec §5.1).
// Each external agent CLI (Codex, Claude Code, …) is wrapped by an
// Adapter that runs it headless and normalizes its JSON output stream
// into Conveyor events. The surface is deliberately small so community
// adapters are cheap to add.
package adapter

import (
	"context"
	"encoding/json"
	"time"
)

// EventKind enumerates the normalized event stream. Warning is
// explicitly nonterminal; Done and Error are the only terminal kinds.
type EventKind string

const (
	EventAssistantText EventKind = "assistant_text"
	EventToolCall      EventKind = "tool_call"
	EventToolResult    EventKind = "tool_result"
	EventTokenUsage    EventKind = "token_usage"
	EventWarning       EventKind = "warning"
	// EventSessionStart carries the harness's session identity in
	// SessionRef, pushed by the adapter as the harness announces it —
	// never scraped from session directories afterward (spec §8.3
	// note 1). It is what Resume() takes.
	EventSessionStart EventKind = "session_start"
	EventDone         EventKind = "done"
	EventError        EventKind = "error"
)

const (
	PhaseMain            = "main"
	PhaseHandoffResume   = "handoff_resume"
	PhaseHandoffFallback = "handoff_fallback"
	PhaseJob             = "job"
)

// Event is one normalized item from a harness run.
type Event struct {
	Kind       EventKind       `json:"kind"`
	Phase      string          `json:"phase,omitempty"`       // main | handoff_resume | handoff_fallback | job
	Text       string          `json:"text,omitempty"`        // assistant_text
	Tool       string          `json:"tool,omitempty"`        // tool_call / tool_result
	SessionRef string          `json:"session_ref,omitempty"` // session_start
	Payload    json.RawMessage `json:"payload,omitempty"`     // raw harness-specific detail
	Usage      *TokenUsage     `json:"usage,omitempty"`       // token_usage
	Err        string          `json:"err,omitempty"`         // error
	At         time.Time       `json:"at"`
}

type TokenUsage struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// ToolPolicy is the per-stage tool policy mapped by the adapter into the
// harness's native permission configuration (spec §8.5 "responsibility
// seam"): sandbox mode plus explicit allow and deny decisions — per job,
// never whatever the user configured interactively.
type ToolPolicy struct {
	// Commands are argv prefixes, matching Codex execpolicy semantics.
	// YAML example: [["git"], ["go", "test"]]. An allow entry permits
	// that prefix; it is not a deny-by-default whitelist. A deny entry blocks
	// its prefix, and deny wins when both match.
	AllowedCommands [][]string `json:"allowed_commands,omitempty" yaml:"allowed_commands"`
	DeniedCommands  [][]string `json:"denied_commands,omitempty" yaml:"denied_commands"`
	// NetworkAllow reserves the cross-runner contract for per-stage egress
	// (spec §18). Phase 1 config rejects non-empty values because its local
	// runner cannot enforce them yet; a declared policy must never be a no-op.
	NetworkAllow []string `json:"network_allow,omitempty" yaml:"network_allow"`
}

// RunSpec is the input to a single harness run.
type RunSpec struct {
	Workdir      string // deterministic per task: /conveyor/jobs/task-<id>/… (spec §8.3)
	Prompt       string
	ContextFiles []string
	Policy       ToolPolicy
	BudgetUSD    float64
}

// Capabilities reports what a harness supports; the router and the
// continuity layer (snapshot vs. native resume, spec §8.3) consult it.
type Capabilities struct {
	MultiRepo     bool
	Resume        bool
	JSONStream    bool
	AuthModes     []string // personal_sub | team_sub | api
	NativeSandbox bool     // robust enough for Tier B confinement (spec §8.5)
}

// Adapter wraps one harness CLI.
//
// Run and Resume return an event channel that closes after a terminal
// EventDone or EventError. Session identity for Resume is captured via
// the harness's hook mechanism during the run — pushed authoritatively,
// never scraped from session directories afterward (spec §8.3 note 1).
type Adapter interface {
	Name() string
	Run(ctx context.Context, spec RunSpec) (<-chan Event, error)
	Resume(ctx context.Context, sessionRef string, feedback string) (<-chan Event, error)
	Capabilities() Capabilities
}
