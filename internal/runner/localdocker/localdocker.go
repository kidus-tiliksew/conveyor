// Package localdocker implements the Phase 1 runner: a daemon on a
// developer machine that runs jobs in containers (spec §3.2, §8.5
// Tier A). Job worktrees and the bare cache are the only host mounts —
// never the home directory, never the Docker socket.
//
// Phase 1 shells out to the docker CLI rather than vendoring the Docker
// SDK; the runner protocol is the stable surface, not the transport.
package localdocker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/runner"
)

type Runner struct {
	// Binary is docker or podman (rootless preferred, spec §8.5).
	Binary string

	mu   sync.Mutex
	jobs map[runner.JobHandle]jobInfo
}

type jobInfo struct {
	jobID         string
	controlDir    string
	credentialDir string
}

func New() *Runner {
	return &Runner{Binary: "docker", jobs: map[runner.JobHandle]jobInfo{}}
}

func (r *Runner) Name() string { return "local-docker" }

func (r *Runner) StartJob(ctx context.Context, spec runner.StartJobSpec) (runner.JobHandle, error) {
	credentialDir, err := stageCredentials(spec)
	if err != nil {
		return "", err
	}
	cleanupCredentials := true
	defer func() {
		if cleanupCredentials && credentialDir != "" {
			_ = os.RemoveAll(credentialDir)
		}
	}()

	args := []string{
		"run", "--detach",
		"--name", "conveyor-job-" + spec.JobID,
		"--label", "conveyor.task=" + spec.TaskID,
		"--label", "conveyor.job=" + spec.JobID,
		// Commits inside the sandbox need an identity; the branch is
		// attributed to the factory, not to whoever runs the daemon.
		"--env", "GIT_AUTHOR_NAME=Conveyor",
		"--env", "GIT_AUTHOR_EMAIL=agent@conveyor.local",
		"--env", "GIT_COMMITTER_NAME=Conveyor",
		"--env", "GIT_COMMITTER_EMAIL=agent@conveyor.local",
		// TODO(phase1-followup): per-stage egress policy via container
		// network config (spec §18); default bridge for now — the
		// harness needs its vendor API.
	}
	for _, wt := range spec.Worktrees {
		mode := "rw"
		if wt.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--volume", fmt.Sprintf("%s:%s:%s", wt.HostPath, wt.SandboxPath, mode))
	}
	args = append(args, "--volume", spec.ControlDir+":"+spec.ControlDir+":rw")
	if credentialDir != "" {
		args = append(args, "--volume",
			credentialDir+":"+filepath.Join("/conveyor/creds", spec.Harness)+":ro")
	}
	// TODO(phase1): resolve spec.SecretRefs via the secrets backend and
	// inject as env/files at boot — values never travel in this spec
	// (spec §10.1). Per-job credentials, rotated on resume (spec §6.2).

	// The image's ENTRYPOINT is already conveyor-shim (images/base), so
	// only its flags follow the image name — repeating the binary here
	// would become a positional arg that stops the shim's flag parsing.
	args = append(args, spec.Image,
		"--harness", spec.Harness,
		"--workdir", spec.Workdir,
		"--control", spec.ControlDir,
	)

	out, err := exec.CommandContext(ctx, r.Binary, args...).CombinedOutput()
	if err != nil {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{RuntimeError: strings.TrimSpace(string(out))},
			Err:         err,
		}
	}
	h := runner.JobHandle(strings.TrimSpace(string(out)))
	r.mu.Lock()
	r.jobs[h] = jobInfo{jobID: spec.JobID, controlDir: spec.ControlDir, credentialDir: credentialDir}
	r.mu.Unlock()
	cleanupCredentials = false
	return h, nil
}

// StreamLogs follows the container's combined output. The channel
// closes when the container exits.
func (r *Runner) StreamLogs(ctx context.Context, h runner.JobHandle) (<-chan runner.LogEvent, error) {
	r.mu.Lock()
	info, ok := r.jobs[h]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job handle %q", h)
	}

	cmd := exec.CommandContext(ctx, r.Binary, "logs", "--follow", string(h))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker logs: %w", err)
	}

	ch := make(chan runner.LogEvent, 256)
	errCh := make(chan error, 3)
	var wg sync.WaitGroup
	for _, pipe := range []interface{ Read([]byte) (int, error) }{stdout, stderr} {
		wg.Add(1)
		go func(rd interface{ Read([]byte) (int, error) }) {
			defer wg.Done()
			sc := bufio.NewScanner(rd)
			sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
			for sc.Scan() {
				ch <- runner.LogEvent{
					JobID: info.jobID,
					Line:  sc.Text(),
					At:    time.Now().UnixMilli(),
				}
			}
			if err := sc.Err(); err != nil {
				errCh <- err
			}
		}(pipe)
	}
	go func() {
		wg.Wait()
		if err := cmd.Wait(); err != nil {
			errCh <- err
		}
		close(errCh)
		for err := range errCh {
			ch <- runner.LogEvent{JobID: info.jobID, Err: err.Error(), At: time.Now().UnixMilli()}
		}
		close(ch)
	}()
	return ch, nil
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

// CollectArtifacts waits for the container to exit and gathers what the
// shim left in the control dir. Commits are read host-side by the
// dispatcher from the worktree, not here — the runner doesn't know the
// repo layout.
func (r *Runner) CollectArtifacts(ctx context.Context, h runner.JobHandle) (runner.Artifacts, error) {
	r.mu.Lock()
	info, ok := r.jobs[h]
	r.mu.Unlock()
	if !ok {
		return runner.Artifacts{}, fmt.Errorf("unknown job handle %q", h)
	}
	defer func() {
		r.mu.Lock()
		delete(r.jobs, h)
		r.mu.Unlock()
		if info.credentialDir != "" {
			_ = os.RemoveAll(info.credentialDir)
		}
	}()

	out, err := exec.CommandContext(ctx, r.Binary, "wait", string(h)).Output()
	if err != nil {
		_ = r.removeContainer(h, true)
		return runner.Artifacts{}, fmt.Errorf("docker wait: %w", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		_ = r.removeContainer(h, true)
		return runner.Artifacts{}, fmt.Errorf("docker wait output %q: %w", out, err)
	}

	art := runner.Artifacts{ExitCode: code}
	if p := filepath.Join(info.controlDir, "events.jsonl"); fileExists(p) {
		art.EventLog = p
	}
	if p := filepath.Join(info.controlDir, "handoff.json"); fileExists(p) {
		art.HandoffSnapshot = p
	}
	// The Phase 1 shim is a one-shot PID 1, so its containers are job-TTL.
	// Worktrees remain persistent across attempts; task-TTL warm container
	// reuse arrives with the resume-fidelity work (spec §8.3, §20.2).
	if err := r.removeContainer(h, false); err != nil {
		return art, err
	}
	return art, nil
}

func (r *Runner) removeContainer(h runner.JobHandle, force bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, string(h))
	out, err := exec.CommandContext(cleanupCtx, r.Binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, out)
	}
	return nil
}

func stageCredentials(spec runner.StartJobSpec) (string, error) {
	if spec.CredentialsDir == "" {
		return "", nil
	}
	if spec.CredentialStageDir == "" {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: "credential staging directory is required"},
			Err:         fmt.Errorf("credential staging directory is required"),
		}
	}
	if spec.Harness != "codex" {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: fmt.Sprintf("credential staging is not defined for harness %q", spec.Harness)},
			Err:         fmt.Errorf("unsupported harness credentials %q", spec.Harness),
		}
	}

	src := filepath.Join(spec.CredentialsDir, "auth.json")
	in, err := os.Open(src)
	if err != nil {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: fmt.Sprintf("read codex auth.json: %v", err)},
			Err:         err,
		}
	}
	defer in.Close()

	if err := os.MkdirAll(spec.CredentialStageDir, 0o700); err != nil {
		return "", &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	stage, err := os.MkdirTemp(spec.CredentialStageDir, "job-creds-")
	if err != nil {
		return "", &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()

	out, err := os.OpenFile(filepath.Join(stage, "auth.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return "", &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	if err := out.Close(); err != nil {
		return "", &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	cleanup = false
	return stage, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
