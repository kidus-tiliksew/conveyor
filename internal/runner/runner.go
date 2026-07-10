// Package runner defines the runner protocol (spec §3.2). A runner is a
// backend that provisions sandboxes: local Docker in Phase 1, Kubernetes
// in Phase 3. All runners implement this one protocol, so "cloud or
// local" is a per-job scheduling decision, not an architectural one.
package runner

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// JobHandle identifies a running job on a runner.
type JobHandle string

// Signal controls a running job.
type Signal string

const (
	SignalPause  Signal = "pause"
	SignalResume Signal = "resume"
	SignalKill   Signal = "kill"
)

// WorktreeMount maps a host path into the sandbox.
//
// TODO(phase1-followup): the spec wants deterministic in-sandbox paths
// (/conveyor/jobs/task-<id>/…, §8.3 note 3) and a read-only bare cache
// (§8.1). Phase 1 mounts host paths at identical in-sandbox paths and
// the cache read-write, because worktree .git files link to the cache
// by absolute host path, and commits must write objects and branch refs
// into the shared store. Fixing both means rewriting worktree gitdir
// links for the sandbox namespace and carving out rw mounts for
// objects/ refs/ worktrees/ — deferred until after the loop runs.
type WorktreeMount struct {
	HostPath    string
	SandboxPath string
	ReadOnly    bool
}

// StartJobSpec is the runner protocol's dispatch payload. Secrets travel
// as references only; the runner resolves them at sandbox boot (spec §10.1).
type StartJobSpec struct {
	JobID     string
	TaskID    string
	Image     string
	Worktrees []WorktreeMount
	// Workdir is the in-sandbox working directory for the harness
	// (the task's worktree).
	Workdir string
	// ControlDir (host == sandbox path) carries prompt.txt in and
	// events.jsonl / handoff.json / harness session state out.
	ControlDir string
	// CredentialsDir is the trusted host source for the harness login
	// (e.g. ~/.codex). It is never mounted directly. The runner copies
	// only the adapter-approved files into CredentialStageDir and mounts
	// that job-specific staging directory read-only (spec §5.2, §8.5).
	CredentialsDir string
	// CredentialStageDir is a runner-private host directory outside the
	// worktree mount. Empty is invalid when CredentialsDir is set.
	CredentialStageDir string
	SecretRefs         []string
	BudgetUSD          float64
	Policy             adapter.ToolPolicy
	Harness            string // which adapter the shim invokes inside the sandbox
	SandboxTTL         string // job | task | <duration> (spec §6.2)
}

// LogEvent is one item of the job's log stream (tool calls, tokens,
// stdout), already redacted at the sandbox edge by the shim (spec §10.3).
type LogEvent struct {
	JobID string
	Line  string
	Err   string // runner-side stream failure; never harness output
	At    int64  // unix millis
}

// BootDiagnostics makes "sandbox failed to boot" a first-class state
// with structured detail (spec §6.2), not a stderr dump.
type BootDiagnostics = core.BootDiagnostics

// BootError carries structured diagnostics across the runner boundary.
// Dispatchers persist Diagnostics on the job record before parking the
// task.
type BootError struct {
	Diagnostics BootDiagnostics
	Err         error
}

func (e *BootError) Error() string {
	if e.Err == nil {
		return "sandbox boot failed"
	}
	return fmt.Sprintf("sandbox boot failed: %v", e.Err)
}

func (e *BootError) Unwrap() error { return e.Err }

// Artifacts is what CollectArtifacts returns: commits made, files,
// screenshots, reports, plus the handoff snapshot (spec §8.3).
// CollectArtifacts blocks until the job's container exits.
type Artifacts struct {
	ExitCode        int
	Commits         []string
	Files           []string
	EventLog        string // path to events.jsonl in the control dir
	HandoffSnapshot string // path to the snapshot JSON artifact, if written
	SessionArchive  string // native session dir archive, if the harness supports resume
}

// Runner is the single protocol every backend implements:
//
//	StartJob(image, worktreeSet, secretRefs, prompt, budget, policy) → jobHandle
//	StreamLogs(jobHandle) → event stream
//	Signal(jobHandle, pause|resume|kill)
//	CollectArtifacts(jobHandle) → commits, files, screenshots, reports
type Runner interface {
	Name() string
	StartJob(ctx context.Context, spec StartJobSpec) (JobHandle, error)
	StreamLogs(ctx context.Context, h JobHandle) (<-chan LogEvent, error)
	Signal(ctx context.Context, h JobHandle, s Signal) error
	CollectArtifacts(ctx context.Context, h JobHandle) (Artifacts, error)
}
