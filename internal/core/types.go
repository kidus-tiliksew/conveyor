// Package core defines the shared domain types for Conveyor: tasks, jobs,
// pipeline stages, and their states. These mirror the data model in
// conveyor-spec.md §16. Postgres is the Phase 2 source of truth; the memory
// store remains an explicit test/development implementation.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Stage is a pipeline stage (spec §4). Phase 1 exercises only Implement;
// the rest are declared so task records and routing config are stable
// from day one.
type Stage string

const (
	StageTriage    Stage = "triage"
	StageSpec      Stage = "spec"
	StageImplement Stage = "implement"
	StageReview    Stage = "review"
	StageVerify    Stage = "verify"
	StageGate      Stage = "gate"
	StageMerge     Stage = "merge"
	StageMonitor   Stage = "monitor"
)

// EscalationLevel is the degree of human involvement (spec §13.1).
type EscalationLevel string

const (
	L0 EscalationLevel = "L0" // fully automatic
	L1 EscalationLevel = "L1" // one-click approve
	L2 EscalationLevel = "L2" // spec review + PR review
	L3 EscalationLevel = "L3" // interactive pairing
)

// TaskState is the denormalized current state of a task.
type TaskState string

const (
	TaskClaiming TaskState = "claiming"
	TaskQueued   TaskState = "queued"
	TaskRunning  TaskState = "running"
	TaskAwaiting TaskState = "awaiting_human"
	TaskApproved TaskState = "approved"
	TaskMerged   TaskState = "merged"
	TaskClosed   TaskState = "closed"
	TaskParked   TaskState = "parked"
)

// JobState includes sandbox boot failure as a first-class state with
// structured diagnostics (spec §6.2) — not a generic "failed".
type JobState string

const (
	JobPending         JobState = "pending"
	JobBooting         JobState = "booting"
	JobRunning         JobState = "running"
	JobPaused          JobState = "paused"
	JobDone            JobState = "done"
	JobFailed          JobState = "failed"
	JobSandboxBootFail JobState = "sandbox_boot_failed"
)

// BootDiagnostics is the structured explanation for a sandbox boot
// failure (spec §6.2). It is stored on the job so the CLI/API can show
// the cause without relying on conveyord's process log.
type BootDiagnostics struct {
	ImageBuildLog   string   `json:"image_build_log,omitempty"`
	ValidationError string   `json:"validation_error,omitempty"`
	RuntimeError    string   `json:"runtime_error,omitempty"`
	MissingEnvVars  []string `json:"missing_env_vars,omitempty"`
}

// Task is a unit of intended change (spec §2). One task spans many jobs.
type Task struct {
	ID           string          `json:"id"`
	Workspace    string          `json:"workspace"`
	Source       string          `json:"source"` // provenance: github:<slug>#<n>, cli, cron, monitor (spec §9)
	Title        string          `json:"title"`
	Body         string          `json:"body"`  // free-form description; becomes part of the prompt
	Class        string          `json:"class"` // bug | feature | chore
	Level        EscalationLevel `json:"level"`
	Repo         string          `json:"repo"` // repo name within the workspace (single-repo in Phase 1; task_repos in Phase 3)
	BaseBranch   string          `json:"base_branch"`
	Branch       string          `json:"branch"` // conveyor/task-<id>
	State        TaskState       `json:"state"`
	ParentTaskID string          `json:"parent_task_id,omitempty"` // stacked tasks (spec §8.6)
	CreatedAt    time.Time       `json:"created_at"`
}

// NewTaskID returns a short, human-typeable ID: date prefix for
// eyeballing, random suffix so concurrent submissions never collide.
func NewTaskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("060102") + "-" + hex.EncodeToString(b)
}

// Job is one execution of one stage in one sandbox by one harness.
type Job struct {
	ID           string   `json:"id"`
	TaskID       string   `json:"task_id"`
	Stage        Stage    `json:"stage"`
	Harness      string   `json:"harness"`
	ModelTier    string   `json:"model_tier"`
	CredentialID string   `json:"credential_id,omitempty"`
	AuthMode     string   `json:"auth_mode,omitempty"`
	Runner       string   `json:"runner"`
	SandboxRef   string   `json:"sandbox_ref,omitempty"`
	PackVersion  string   `json:"pack_version,omitempty"`
	Confinement  string   `json:"confinement"` // tierA | tierB | tierC, recorded per job (spec §8.5)
	BudgetUSD    float64  `json:"budget_usd"`
	CostUSD      float64  `json:"cost_usd"`
	TokensIn     int64    `json:"tokens_in"`
	TokensOut    int64    `json:"tokens_out"`
	State        JobState `json:"state"`
	// BootDiagnostics is populated only when State is
	// JobSandboxBootFail.
	BootDiagnostics *BootDiagnostics `json:"boot_diagnostics,omitempty"`
	StartedAt       time.Time        `json:"started_at"`
	EndedAt         time.Time        `json:"ended_at,omitempty"`
}

// MarshalJSON makes the wire contract honor ended_at's optionality. The
// standard encoder does not consider a zero time.Time empty, so relying on
// omitempty alone serializes year 1 and freezes running-job durations at 0s.
func (j Job) MarshalJSON() ([]byte, error) {
	type jobAlias Job
	wired := struct {
		jobAlias
		EndedAt *time.Time `json:"ended_at,omitempty"`
	}{jobAlias: jobAlias(j)}
	if !j.EndedAt.IsZero() {
		wired.EndedAt = &j.EndedAt
	}
	return json.Marshal(wired)
}

// ActorRole is recorded on every append-only event and intervention from the
// beginning of Phase 2 so later RBAC adds enforcement to an existing audit
// identity model instead of retrofitting attribution (spec §16, §18.1).
type ActorRole string

const (
	ActorSystem ActorRole = "system"
	ActorHuman  ActorRole = "human"
	ActorAgent  ActorRole = "agent"
	ActorRunner ActorRole = "runner"
)

func (action InterventionAction) Valid() bool {
	switch action {
	case InterventionApprove, InterventionReject, InterventionRedirect, InterventionPull:
		return true
	default:
		return false
	}
}

// Event is the append-only source of truth for task state. Tasks and jobs are
// transactional projections optimized for queries (spec §3.1, §16).
type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	JobID     string          `json:"job_id,omitempty"`
	Kind      string          `json:"kind"`
	ActorID   string          `json:"actor_id"`
	ActorRole ActorRole       `json:"actor_role"`
	Payload   json.RawMessage `json:"payload"`
	At        time.Time       `json:"at"`
}

type InterventionAction string

const (
	InterventionApprove  InterventionAction = "approve"
	InterventionReject   InterventionAction = "reject"
	InterventionRedirect InterventionAction = "redirect"
	InterventionPull     InterventionAction = "pull_to_local"
)

// Intervention is a structured human decision from the review queue (spec
// §13.2). ReasonCode is intentionally data rather than an enum so workspaces
// can add taxonomy without a deploy; the API validates the required baseline.
type Intervention struct {
	ID         int64              `json:"id"`
	TaskID     string             `json:"task_id"`
	JobID      string             `json:"job_id,omitempty"`
	ActorID    string             `json:"actor_id"`
	ActorRole  ActorRole          `json:"actor_role"`
	Action     InterventionAction `json:"action"`
	ReasonCode string             `json:"reason_code"`
	Comment    string             `json:"comment"`
	At         time.Time          `json:"at"`
}

func InterventionNextState(action InterventionAction) (TaskState, bool) {
	switch action {
	case InterventionApprove:
		return TaskApproved, true
	case InterventionReject:
		return TaskClosed, true
	case InterventionRedirect:
		return TaskQueued, true
	case InterventionPull:
		return "", false
	default:
		return "", false
	}
}

type RedactionStats struct {
	Exact   int64 `json:"exact"`
	Encoded int64 `json:"encoded"`
	Pattern int64 `json:"pattern"`
	Entropy int64 `json:"entropy"`
}

func (s RedactionStats) Total() int64 { return s.Exact + s.Encoded + s.Pattern + s.Entropy }

func (s *RedactionStats) Add(other RedactionStats) {
	s.Exact += other.Exact
	s.Encoded += other.Encoded
	s.Pattern += other.Pattern
	s.Entropy += other.Entropy
}

type Transcript struct {
	JobID          string         `json:"job_id"`
	URI            string         `json:"uri"`
	RedactionStats RedactionStats `json:"redaction_stats"`
	CreatedAt      time.Time      `json:"created_at"`
}

// JSONPayload is the single fallback contract for JSON stored inside events.
// Domain payloads should always marshal; if a future caller supplies an
// unsupported value, preserve the event without panicking the control plane.
func JSONPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"marshal_error":true}`)
	}
	return data
}
