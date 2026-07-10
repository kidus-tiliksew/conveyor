// Package core defines the shared domain types for Conveyor: tasks, jobs,
// pipeline stages, and their states. These mirror the data model in
// conveyor-spec.md §16; Phase 1 keeps them in memory, Phase 2 moves them
// to Postgres (event-sourced, pgx + sqlc + River).
package core

import (
	"crypto/rand"
	"encoding/hex"
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
	TaskQueued   TaskState = "queued"
	TaskRunning  TaskState = "running"
	TaskAwaiting TaskState = "awaiting_human"
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

// Task is a unit of intended change (spec §2). One task spans many jobs.
type Task struct {
	ID           string
	Workspace    string
	Source       string // provenance: github:<slug>#<n>, cli, cron, monitor (spec §9)
	Title        string
	Body         string // free-form description; becomes part of the prompt
	Class        string // bug | feature | chore
	Level        EscalationLevel
	Repo         string // repo name within the workspace (single-repo in Phase 1; task_repos in Phase 3)
	BaseBranch   string
	Branch       string // conveyor/task-<id>
	State        TaskState
	ParentTaskID string // stacked tasks (spec §8.6)
	CreatedAt    time.Time
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
	ID          string
	TaskID      string
	Stage       Stage
	Harness     string
	ModelTier   string
	Runner      string
	SandboxRef  string
	PackVersion string
	Confinement string // tierA | tierB | tierC, recorded per job (spec §8.5)
	BudgetUSD   float64
	CostUSD     float64
	TokensIn    int64
	TokensOut   int64
	State       JobState
	StartedAt   time.Time
	EndedAt     time.Time
}
