// Package localdocker implements the standalone local runner on a developer
// machine and provisions Tier A containers (spec §3.2, §8.5). Isolated
// task clones and the job control directory are the only mutable host mounts
// — never the home directory, bare cache, or Docker socket.
//
// The local runner shells out to the docker CLI rather than vendoring the Docker
// SDK; the runner protocol is the stable surface, not the transport.
package localdocker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/jobartifact"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/secrets"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
)

type Runner struct {
	// Binary is docker or podman (rootless preferred, spec §8.5).
	Binary string
	// SecretResolver runs only on the trusted runner host. Resolved values
	// enter Docker through a short-lived env file, never job specs or args.
	SecretResolver secrets.Resolver
	SecretPolicies map[string]secrets.SetPolicy

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
	controlPath := spec.ControlPath
	if controlPath == "" {
		controlPath = spec.ControlDir
	}
	secretEnv, secretNames, err := r.stageSecrets(ctx, spec)
	if err != nil {
		return "", err
	}
	if secretEnv != "" {
		defer os.RemoveAll(filepath.Dir(secretEnv))
	}
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
	credentialFile := ""
	if credentialDir != "" {
		layout, _ := adapter.CredentialLayoutFor(spec.Harness)
		credentialFile = filepath.Join("/conveyor/creds", spec.Harness, layout.FileName)
	}
	if err := writeRedactionManifest(spec.ControlDir, redact.Manifest{
		EnvNames: secretNames, CredentialFile: credentialFile,
	}); err != nil {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: "stage redaction manifest: " + err.Error()},
			Err:         err,
		}
	}

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
	if secretEnv != "" {
		args = append(args, "--env-file", secretEnv)
	}
	args = append(args, "--volume", spec.ControlDir+":"+controlPath+":rw")
	if credentialDir != "" {
		args = append(args, "--volume",
			credentialDir+":"+filepath.Join("/conveyor/creds", spec.Harness)+":ro")
	}
	if err := writePolicy(spec.ControlDir, spec.Policy); err != nil {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: "stage tool policy: " + err.Error()},
			Err:         err,
		}
	}

	// The image's ENTRYPOINT is already conveyor-shim (images/base), so
	// only its flags follow the image name — repeating the binary here
	// would become a positional arg that stops the shim's flag parsing.
	args = append(args, spec.Image,
		"--harness", spec.Harness,
		"--workdir", spec.Workdir,
		"--control", controlPath,
		"--task", spec.TaskID,
		"--job", spec.JobID,
		"--budget-usd", strconv.FormatFloat(spec.BudgetUSD, 'f', -1, 64),
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

func (r *Runner) stageSecrets(ctx context.Context, spec runner.StartJobSpec) (string, []string, error) {
	if len(spec.SecretRefs) == 0 {
		return "", nil, nil
	}
	if r.SecretResolver == nil {
		return "", nil, &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: "secret references supplied but no resolver is configured"},
			Err:         fmt.Errorf("secret resolver is required"),
		}
	}
	if spec.SecretStageDir == "" {
		return "", nil, &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: "secret staging directory is required"},
			Err:         fmt.Errorf("secret staging directory is required"),
		}
	}
	if err := os.MkdirAll(spec.SecretStageDir, 0o700); err != nil {
		return "", nil, &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	stage, err := os.MkdirTemp(spec.SecretStageDir, "job-secrets-")
	if err != nil {
		return "", nil, &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()

	values := make(map[string]string, len(spec.SecretRefs))
	for _, raw := range spec.SecretRefs {
		ref, err := secrets.ParseRef(raw)
		if err != nil {
			return "", nil, secretBootError(err)
		}
		policy, ok := r.SecretPolicies[secrets.PolicyKey(ref)]
		if !ok {
			return "", nil, secretBootError(fmt.Errorf("secret set %s has no delivery policy", secrets.PolicyKey(ref)))
		}
		if !policy.LocalEligible {
			return "", nil, secretBootError(fmt.Errorf("secret set %s is not local_eligible", secrets.PolicyKey(ref)))
		}
		if !secrets.ValidEnvName(ref.Name) {
			return "", nil, secretBootError(fmt.Errorf("secret %s is not a valid environment variable", ref.Name))
		}
		if _, duplicate := values[ref.Name]; duplicate {
			return "", nil, secretBootError(fmt.Errorf("duplicate injected environment name %s", ref.Name))
		}
		value, err := r.SecretResolver.Resolve(ctx, ref)
		if err != nil {
			return "", nil, secretBootError(err)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", nil, secretBootError(fmt.Errorf("secret %s is not a single-line environment value", ref.Name))
		}
		values[ref.Name] = value
	}

	path := filepath.Join(stage, "secrets.env")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(out, "%s=%s\n", name, values[name]); err != nil {
			_ = out.Close()
			return "", nil, &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
		}
	}
	if err := out.Close(); err != nil {
		return "", nil, &runner.BootError{Diagnostics: runner.BootDiagnostics{RuntimeError: err.Error()}, Err: err}
	}
	cleanup = false
	return path, names, nil
}

func writeRedactionManifest(controlDir string, manifest redact.Manifest) error {
	if controlDir == "" {
		return fmt.Errorf("control directory is required")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(controlDir, redact.ManifestFile), data, 0o600)
}

func secretBootError(err error) error {
	return &runner.BootError{
		Diagnostics: runner.BootDiagnostics{ValidationError: err.Error()},
		Err:         err,
	}
}

func writePolicy(controlDir string, policy adapter.ToolPolicy) error {
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(controlDir, "policy.json"), data, 0o600)
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
	if p, err := jobartifact.EventLogPath(info.controlDir, info.jobID); err == nil && fileExists(p) {
		art.EventLog = p
	}
	if p, err := snapshot.Path(info.controlDir, info.jobID); err == nil && fileExists(p) {
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
	layout, ok := adapter.CredentialLayoutFor(spec.Harness)
	if !ok {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: fmt.Sprintf("credential staging is not defined for harness %q", spec.Harness)},
			Err:         fmt.Errorf("unsupported harness credentials %q", spec.Harness),
		}
	}

	src := filepath.Join(spec.CredentialsDir, layout.FileName)
	in, err := os.Open(src)
	if err != nil {
		return "", &runner.BootError{
			Diagnostics: runner.BootDiagnostics{ValidationError: fmt.Sprintf("read %s %s: %v", spec.Harness, layout.FileName, err)},
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

	out, err := os.OpenFile(filepath.Join(stage, layout.FileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
