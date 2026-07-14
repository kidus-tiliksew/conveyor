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

func InitialStage(level EscalationLevel) Stage {
	if level == "" {
		return StageImplement
	}
	return StageTriage
}

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

type JobState string

const (
	JobPending JobState = "pending"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// Task is a unit of intended change (spec §2). One task spans many jobs.
type Task struct {
	ID            string          `json:"id"`
	Workspace     string          `json:"workspace"`
	Source        string          `json:"source"` // provenance: github:<slug>#<n>, cli, cron, monitor (spec §9)
	IntakeKey     string          `json:"-"`      // workspace-scoped MCP retry key (spec §21.5)
	Title         string          `json:"title"`
	Body          string          `json:"body"`  // free-form description; becomes part of the prompt
	Class         string          `json:"class"` // bug | feature | chore
	Level         EscalationLevel `json:"level"`
	Repo          string          `json:"repo"` // repo name within the workspace; multi-repo sets are Phase 8
	BaseBranch    string          `json:"base_branch"`
	Branch        string          `json:"branch"` // conveyor/task-<id>
	State         TaskState       `json:"state"`
	NextStage     Stage           `json:"next_stage,omitempty"`     // durable pipeline transition selected at the preceding gate
	RecoveryStage Stage           `json:"recovery_stage,omitempty"` // explicit human redirect/pull target while the pipeline is halted
	ParentTaskID  string          `json:"parent_task_id,omitempty"` // stacked tasks (spec §8.6)
	FeatureID     string          `json:"feature_id,omitempty"`     // requirements-tree assignment (spec §21.4)
	CreatedAt     time.Time       `json:"created_at"`
}

// NewTaskID returns a short, human-typeable ID: date prefix for
// eyeballing, random suffix so concurrent submissions never collide.
func NewTaskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("060102") + "-" + hex.EncodeToString(b)
}

// Job is one execution of one pipeline stage.
type Job struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	Stage       Stage     `json:"stage"`
	Harness     string    `json:"harness"`
	ModelTier   string    `json:"model_tier"`
	AuthMode    string    `json:"auth_mode,omitempty"`
	Runner      string    `json:"runner"`
	PackVersion string    `json:"pack_version,omitempty"`
	Confinement string    `json:"confinement"`
	CostUSD     float64   `json:"cost_usd"`
	TokensIn    int64     `json:"tokens_in"`
	TokensOut   int64     `json:"tokens_out"`
	State       JobState  `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
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

type WorkOrderState string

const (
	WorkOrderQueued    WorkOrderState = "queued"
	WorkOrderClaimed   WorkOrderState = "claimed"
	WorkOrderSubmitted WorkOrderState = "submitted"
	WorkOrderCompleted WorkOrderState = "completed"
	WorkOrderCancelled WorkOrderState = "cancelled"
)

// WorkOrder is the durable protocol boundary between Conveyor and an
// operator-owned implementation or review agent (spec §21.4).
type WorkOrder struct {
	ID              string         `json:"id"`
	TaskID          string         `json:"task_id"`
	JobID           string         `json:"job_id"`
	Stage           Stage          `json:"stage"`
	State           WorkOrderState `json:"state"`
	ClaimantID      string         `json:"claimed_by,omitempty"`
	SessionID       string         `json:"session_id,omitempty"`
	ClientTokenHash string         `json:"-"`
	Agent           string         `json:"agent,omitempty"`
	Model           string         `json:"model,omitempty"`
	LeaseExpiresAt  time.Time      `json:"lease_expires_at,omitempty"`
	Progress        string         `json:"progress,omitempty"`
	CostUSD         float64        `json:"cost_usd"`
	TokensIn        int64          `json:"tokens_in"`
	TokensOut       int64          `json:"tokens_out"`
	SelfReported    bool           `json:"self_reported"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type WorkOrderClaim struct {
	SessionID   string
	ClientToken string
	ClaimantID  string
	Agent       string
	Model       string
	Lease       time.Duration
}

type Feature struct {
	ID          string    `json:"id"`
	Workspace   string    `json:"workspace"`
	ParentID    string    `json:"parent_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Artifact struct {
	ID          string    `json:"id"` // sha256 content address
	Workspace   string    `json:"workspace"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	TaskID      string    `json:"task_id,omitempty"`
	FeatureID   string    `json:"feature_id,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SpecVersion is an immutable spec-agent artifact. Approval is recorded on
// the exact version that becomes the implementation contract (spec §4.1).
type SpecVersion struct {
	TaskID          string          `json:"task_id"`
	Version         int             `json:"version"`
	Content         string          `json:"content"`
	AcceptanceCount int             `json:"acceptance_count"`
	Acceptance      json.RawMessage `json:"acceptance"`
	Decomposition   json.RawMessage `json:"decomposition"`
	Approved        bool            `json:"approved"`
	CreatedAt       time.Time       `json:"created_at"`
	ApprovedAt      time.Time       `json:"approved_at,omitempty"`
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
