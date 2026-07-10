// Package localdocker implements the Phase 1 runner: a daemon on a
// developer machine that runs jobs in containers (spec §3.2, §8.5
// Tier A). Job worktrees mount rw, the bare cache ro, and nothing
// else — never the home directory, never the Docker socket.
//
// Phase 1 shells out to the docker CLI rather than vendoring the Docker
// SDK; the runner protocol is the stable surface, not the transport.
package localdocker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/runner"
)

type Runner struct {
	// Binary is docker or podman (rootless preferred, spec §8.5).
	Binary string
}

func New() *Runner { return &Runner{Binary: "docker"} }

func (r *Runner) Name() string { return "local-docker" }

func (r *Runner) StartJob(ctx context.Context, spec runner.StartJobSpec) (runner.JobHandle, error) {
	name := "conveyor-job-" + spec.JobID
	args := []string{
		"run", "--detach", "--name", name,
		// Tier A confinement: no host network by default; per-stage
		// egress policy lands here (spec §18).
		"--label", "conveyor.task=" + spec.TaskID,
		"--label", "conveyor.job=" + spec.JobID,
	}
	for _, wt := range spec.Worktrees {
		mode := "rw"
		if wt.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--volume", fmt.Sprintf("%s:%s:%s", wt.HostPath, wt.SandboxPath, mode))
	}
	// TODO(phase1): resolve spec.SecretRefs via the secrets backend and
	// inject as env/files at boot — values never travel in this spec
	// (spec §10.1). Per-job credentials, rotated on resume (spec §6.2).
	// TODO(phase1): entrypoint is the job shim, which supervises the
	// harness, meters usage, and streams redacted logs (spec §6.3).
	args = append(args, spec.Image, "conveyor-shim", "--harness", spec.Harness)

	out, err := exec.CommandContext(ctx, r.Binary, args...).CombinedOutput()
	if err != nil {
		// Boot failure is a first-class state with structured
		// diagnostics (spec §6.2), surfaced by the orchestrator.
		return "", fmt.Errorf("sandbox boot failed: %w: %s", err, out)
	}
	return runner.JobHandle(strings.TrimSpace(string(out))), nil
}

func (r *Runner) StreamLogs(ctx context.Context, h runner.JobHandle) (<-chan runner.LogEvent, error) {
	// TODO(phase1): docker logs --follow, line-framed into LogEvents.
	return nil, fmt.Errorf("not implemented")
}

func (r *Runner) Signal(ctx context.Context, h runner.JobHandle, s runner.Signal) error {
	var verb string
	switch s {
	case runner.SignalPause:
		verb = "pause"
	case runner.SignalResume:
		verb = "unpause"
	case runner.SignalKill:
		verb = "kill"
	default:
		return fmt.Errorf("unknown signal %q", s)
	}
	out, err := exec.CommandContext(ctx, r.Binary, verb, string(h)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w: %s", verb, err, out)
	}
	return nil
}

func (r *Runner) CollectArtifacts(ctx context.Context, h runner.JobHandle) (runner.Artifacts, error) {
	// TODO(phase1): commits are read from the worktree (host side);
	// handoff snapshot + session archive are copied from the job dir.
	return runner.Artifacts{}, fmt.Errorf("not implemented")
}
