package resumefidelity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	seedPrompt       = `Read decision-context.md and internalize its engineering decision and rationale for a later continuation. Also remember the private continuity marker "amber-orchid-27". The marker is conversation-only: do not write it or any summary to the workspace. Reply exactly: context established`
	checkpointPrompt = `This is a crash checkpoint. Run exactly the shell command "sleep 120" now. Do not do any other work and do not reply until that command completes.`
	handoffSnapshot  = `Handoff snapshot:
- Decision: use lease_epoch as the worker-claim mechanism.
- Rationale: monotonically increasing epochs fence stale workers, and explicit epochs make recovery deterministic under event replay.
- Rejected: heartbeat_only, because a network partition can leave a stale worker active.
- Next action: answer the continuity probe. No private continuity marker was stored in this snapshot.`
	probePrompt = handoffSnapshot + `

Answer the continuity probe from the snapshot and any native conversation context you actually retain. Return only the required JSON object. For continuity_marker, return the exact private marker if native context contains it; otherwise return "unknown". Do not search the filesystem for a marker and do not invent one.`
)

type scenarioSpec struct {
	name           string
	description    string
	probeImage     string
	probeVersion   string
	reuseContainer bool
}

type runner struct {
	config      Config
	runID       string
	workRoot    string
	fixtureDir  string
	artifactDir string
}

// Run executes the full six-probe matrix and writes JSON, Markdown, and raw
// Codex JSONL evidence. A failed scenario is recorded rather than hiding the
// other two results.
func Run(ctx context.Context, cfg Config) (Result, error) {
	cfg = withDefaults(cfg)
	if err := validateConfig(cfg); err != nil {
		return Result{}, err
	}

	started := time.Now().UTC()
	runID := started.Format("20060102T150405Z")
	workRoot := cfg.WorkRoot
	createdTemp := false
	if workRoot == "" {
		var err error
		workRoot, err = os.MkdirTemp("", "conveyor-resume-fidelity-"+runID+"-")
		if err != nil {
			return Result{}, fmt.Errorf("create work root: %w", err)
		}
		createdTemp = true
	}
	if createdTemp && !cfg.KeepWork {
		defer os.RemoveAll(workRoot)
	}

	r := &runner{
		config:      cfg,
		runID:       runID,
		workRoot:    workRoot,
		fixtureDir:  filepath.Join(workRoot, "fixture"),
		artifactDir: filepath.Join(cfg.OutputDir, runID),
	}
	if err := os.MkdirAll(r.artifactDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create artifact dir: %w", err)
	}
	if err := r.createFixture(); err != nil {
		return Result{}, err
	}
	if err := r.preflight(ctx); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(os.Stderr, "resume-fidelity %s: preflight passed (%s -> %s)\n", runID, cfg.BaseVersion, cfg.BumpVersion)

	specs := []scenarioSpec{
		{
			name:           "same_sandbox",
			description:    "SIGKILL Codex and resume its session in the still-running container.",
			probeImage:     cfg.BaseImage,
			probeVersion:   cfg.BaseVersion,
			reuseContainer: true,
		},
		{
			name:         "fresh_sandbox",
			description:  "Kill the container, copy the persisted CODEX_HOME into a new host root, and resume in a fresh container at identical in-container paths.",
			probeImage:   cfg.BaseImage,
			probeVersion: cfg.BaseVersion,
		},
		{
			name:         "version_bump",
			description:  "Seed with the pinned CLI, kill the container, restore CODEX_HOME, and resume with the next minor CLI image.",
			probeImage:   cfg.BumpImage,
			probeVersion: cfg.BumpVersion,
		},
	}

	result := Result{
		SchemaVersion: 1,
		RunID:         runID,
		StartedAt:     started,
		BaseImage:     cfg.BaseImage,
		BaseVersion:   cfg.BaseVersion,
		BumpImage:     cfg.BumpImage,
		BumpVersion:   cfg.BumpVersion,
		HostBoundary:  "The fresh-host condition is a host-equivalent restore boundary on one Docker daemon: the original container is destroyed and session state is copied to a distinct host directory before a fresh container starts. No auth material leaves the local machine.",
		Scoring: ScoringPolicy{
			CoreMaximum:            4,
			ExtendedMaximum:        5,
			ResumeCostCeilingRatio: resumeCostCeiling,
			EffectiveTokenFormula:  "max(input_tokens - cached_input_tokens, 0) + output_tokens",
		},
	}
	for _, spec := range specs {
		fmt.Fprintf(os.Stderr, "resume-fidelity %s: running %s\n", runID, spec.name)
		scenario := r.runScenario(ctx, spec)
		result.Scenarios = append(result.Scenarios, scenario)
		if scenario.Error != "" {
			fmt.Fprintf(os.Stderr, "resume-fidelity %s: %s failed: %s\n", runID, spec.name, scenario.Error)
		} else {
			fmt.Fprintf(os.Stderr, "resume-fidelity %s: %s complete (resume=%d/5 cold=%d/5 ratio=%.2f)\n",
				runID, spec.name, scenario.Resume.Score.Extended, scenario.Cold.Score.Extended, scenario.Comparison.ResumeToColdRatio)
		}
	}
	result.RoutingDefault = routingDefault(result.Scenarios)
	result.FinishedAt = time.Now().UTC()

	jsonPath, markdownPath, err := writeArtifacts(r.artifactDir, result)
	if err != nil {
		return Result{}, err
	}
	result.JSONArtifact = jsonPath
	result.MarkdownArtifact = markdownPath
	var incomplete []string
	for _, scenario := range result.Scenarios {
		if scenario.Error != "" || scenario.Resume.CLIError != "" || scenario.Resume.ParseError != "" ||
			scenario.Cold.CLIError != "" || scenario.Cold.ParseError != "" {
			incomplete = append(incomplete, scenario.Name)
		}
	}
	if len(incomplete) > 0 {
		return result, fmt.Errorf("experiment incomplete in %s; inspect %s", strings.Join(incomplete, ", "), markdownPath)
	}
	return result, nil
}

func withDefaults(cfg Config) Config {
	if cfg.BaseImage == "" {
		cfg.BaseImage = DefaultBaseImage
	}
	if cfg.BumpImage == "" {
		cfg.BumpImage = DefaultBumpImage
	}
	if cfg.BaseVersion == "" {
		cfg.BaseVersion = DefaultBaseVersion
	}
	if cfg.BumpVersion == "" {
		cfg.BumpVersion = DefaultBumpVersion
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = filepath.Join("experiments", "resume-fidelity", "results")
	}
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = 10 * time.Minute
	}
	if cfg.CrashTimeout == 0 {
		cfg.CrashTimeout = 2 * time.Minute
	}
	return cfg
}

func validateConfig(cfg Config) error {
	if cfg.AuthPath == "" {
		return errors.New("Codex auth path is required")
	}
	info, err := os.Stat(cfg.AuthPath)
	if err != nil {
		return fmt.Errorf("stat Codex auth: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Codex auth path %q is not a regular file", cfg.AuthPath)
	}
	if cfg.BaseVersion == cfg.BumpVersion {
		return errors.New("base and bump Codex versions must differ")
	}
	return nil
}

func (r *runner) preflight(ctx context.Context) error {
	if out, err := commandOutput(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("Docker unavailable: %w: %s", err, out)
	}
	for _, pair := range []struct {
		image   string
		version string
	}{{r.config.BaseImage, r.config.BaseVersion}, {r.config.BumpImage, r.config.BumpVersion}} {
		out, err := commandOutput(ctx, "docker", "run", "--rm", "--entrypoint", "codex", pair.image, "--version")
		if err != nil {
			return fmt.Errorf("verify %s: %w: %s", pair.image, err, out)
		}
		want := "codex-cli " + pair.version
		if strings.TrimSpace(out) != want {
			return fmt.Errorf("image %s reports %q, want %q", pair.image, strings.TrimSpace(out), want)
		}
	}
	return nil
}

func (r *runner) runScenario(ctx context.Context, spec scenarioSpec) (result ScenarioResult) {
	result = ScenarioResult{
		Name:            spec.name,
		Description:     spec.description,
		SeedVersion:     r.config.BaseVersion,
		ProbeVersion:    spec.probeVersion,
		ContainerReused: spec.reuseContainer,
		SessionRestored: !spec.reuseContainer,
		SeedEvents:      filepath.ToSlash(filepath.Join(spec.name, "seed.events.jsonl")),
		CrashEvents:     filepath.ToSlash(filepath.Join(spec.name, "crash.events.jsonl")),
	}
	scenarioArtifacts := filepath.Join(r.artifactDir, spec.name)
	if err := os.MkdirAll(scenarioArtifacts, 0o755); err != nil {
		result.Error = err.Error()
		return result
	}

	seedHome := filepath.Join(r.workRoot, spec.name, "seed-home")
	coldHome := filepath.Join(r.workRoot, spec.name, "cold-home")
	for _, home := range []string{seedHome, coldHome} {
		if err := r.stageAuth(home); err != nil {
			result.Error = err.Error()
			return result
		}
	}

	container := r.containerName(spec.name, "seed")
	if err := r.startContainer(ctx, container, r.config.BaseImage, seedHome, coldHome); err != nil {
		result.Error = err.Error()
		return result
	}
	activeContainer := container
	defer func() { _ = removeContainer(activeContainer) }()

	seedEventsPath := filepath.Join(scenarioArtifacts, "seed.events.jsonl")
	sessionRef, err := r.seed(ctx, container, seedHome, seedEventsPath)
	result.SessionRef = sessionRef
	if err != nil {
		result.Error = err.Error()
		return result
	}

	crashEventsPath := filepath.Join(scenarioArtifacts, "crash.events.jsonl")
	result.CrashObserved, err = r.checkpointAndCrash(ctx, container, sessionRef, spec.reuseContainer, crashEventsPath)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	probeHome := seedHome
	if !spec.reuseContainer {
		_ = removeContainer(container)
		activeContainer = ""
		restoredHome := filepath.Join(r.workRoot, spec.name, "restored-home")
		if err := copyTree(seedHome, restoredHome); err != nil {
			result.Error = fmt.Sprintf("restore session home: %v", err)
			return result
		}
		probeHome = restoredHome
		container = r.containerName(spec.name, "probe")
		if err := r.startContainer(ctx, container, spec.probeImage, probeHome, coldHome); err != nil {
			result.Error = err.Error()
			return result
		}
		activeContainer = container
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result.Resume = r.probe(ctx, container, probeHome, "/codex-home", sessionRef, true, filepath.Join(scenarioArtifacts, "resume.events.jsonl"))
	}()
	go func() {
		defer wg.Done()
		result.Cold = r.probe(ctx, container, coldHome, "/cold-home", "", false, filepath.Join(scenarioArtifacts, "cold.events.jsonl"))
	}()
	wg.Wait()
	result.Comparison = compare(result.Resume, result.Cold)
	result.Recommendation = recommendation(result.Comparison)
	return result
}

func (r *runner) createFixture() error {
	if err := os.MkdirAll(r.fixtureDir, 0o755); err != nil {
		return fmt.Errorf("create fixture: %w", err)
	}
	decision := `# Worker claim decision

Use a monotonically increasing lease_epoch for worker claims.

- An epoch fences stale workers after ownership changes.
- Explicit epochs make crash recovery deterministic under event replay.
- Reject heartbeat_only: a network partition can leave a stale worker active.
`
	schema := `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["decision", "rationale", "rejected_alternative", "continuity_marker"],
  "properties": {
    "decision": {"type": "string"},
    "rationale": {"type": "array", "items": {"type": "string"}, "minItems": 2},
    "rejected_alternative": {"type": "string"},
    "continuity_marker": {"type": "string"}
  }
}
`
	if err := os.WriteFile(filepath.Join(r.fixtureDir, "decision-context.md"), []byte(decision), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.fixtureDir, "probe-schema.json"), []byte(schema), 0o644); err != nil {
		return err
	}
	if out, err := commandOutput(context.Background(), "git", "init", "-q", "-b", "main", r.fixtureDir); err != nil {
		return fmt.Errorf("init fixture git repo: %w: %s", err, out)
	}
	return nil
}

func (r *runner) stageAuth(home string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	source, err := os.Open(r.config.AuthPath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(filepath.Join(home, "auth.json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	return errors.Join(copyErr, closeErr)
}

func (r *runner) startContainer(ctx context.Context, name, image, sessionHome, coldHome string) error {
	args := []string{
		"run", "--detach",
		"--name", name,
		"--label", "conveyor.experiment=resume-fidelity",
		"--env", "CODEX_HOME=/codex-home",
		"--env", "HOME=/codex-home",
		"--workdir", "/workspace",
		"--volume", r.fixtureDir + ":/workspace:ro",
		"--volume", sessionHome + ":/codex-home:rw",
		"--volume", coldHome + ":/cold-home:rw",
		"--entrypoint", "sleep",
		image, "infinity",
	}
	out, err := commandOutput(ctx, "docker", args...)
	if err != nil {
		return fmt.Errorf("start %s: %w: %s", name, err, out)
	}
	return nil
}

func (r *runner) seed(ctx context.Context, container, hostHome, eventsPath string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, r.config.CommandTimeout)
	defer cancel()
	args := dockerExecArgs(container, "/codex-home", "codex", "exec",
		"--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "--ignore-rules", "-o", "/codex-home/seed-output.txt", seedPrompt)
	out, err := exec.CommandContext(commandCtx, "docker", args...).CombinedOutput()
	if writeErr := os.WriteFile(eventsPath, out, 0o644); writeErr != nil {
		return "", writeErr
	}
	sessionRef, _ := parseCodexEvents(out)
	if err != nil {
		return sessionRef, fmt.Errorf("seed Codex session: %w: %s", err, tail(out, 1200))
	}
	if sessionRef == "" {
		return "", errors.New("seed Codex session emitted no thread.started thread_id")
	}
	if _, err := os.Stat(filepath.Join(hostHome, "seed-output.txt")); err != nil {
		return "", fmt.Errorf("seed output missing: %w", err)
	}
	return sessionRef, nil
}

func (r *runner) checkpointAndCrash(ctx context.Context, container, sessionRef string, keepContainer bool, eventsPath string) (bool, error) {
	checkpointCtx, cancel := context.WithTimeout(ctx, r.config.CrashTimeout)
	defer cancel()
	args := dockerExecArgs(container, "/codex-home", "sh", "-c",
		`echo $$ > /codex-home/checkpoint.pid; exec "$@"`, "checkpoint",
		"codex", "exec", "resume", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", "--ignore-rules",
		sessionRef, checkpointPrompt)
	cmd := exec.CommandContext(checkpointCtx, "docker", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}

	var output bytes.Buffer
	seen := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer close(done)
		var once sync.Once
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			output.Write(line)
			output.WriteByte('\n')
			if bytes.Contains(line, []byte("command_execution")) && bytes.Contains(line, []byte("sleep 120")) {
				once.Do(func() { close(seen) })
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			done <- scanErr
			return
		}
		waitErr := cmd.Wait()
		if stderr.Len() > 0 {
			output.Write(stderr.Bytes())
			if stderr.Bytes()[stderr.Len()-1] != '\n' {
				output.WriteByte('\n')
			}
		}
		done <- waitErr
	}()

	select {
	case <-seen:
	case waitErr := <-done:
		_ = os.WriteFile(eventsPath, output.Bytes(), 0o644)
		return false, fmt.Errorf("checkpoint exited before sleep tool call: %v: %s", waitErr, tail(output.Bytes(), 1200))
	case <-checkpointCtx.Done():
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return false, fmt.Errorf("checkpoint tool-call timeout; docker exec did not stop: %w", checkpointCtx.Err())
		}
		_ = os.WriteFile(eventsPath, output.Bytes(), 0o644)
		return false, fmt.Errorf("checkpoint tool-call timeout: %w", checkpointCtx.Err())
	}

	var killErr error
	if keepContainer {
		killScript := `kill_tree() { p="$1"; if [ -r "/proc/$p/task/$p/children" ]; then for c in $(cat "/proc/$p/task/$p/children"); do kill_tree "$c"; done; fi; kill -9 "$p" 2>/dev/null || true; }; kill_tree "$(cat /codex-home/checkpoint.pid)"`
		killOut, err := commandOutput(ctx, "docker", "exec", container, "sh", "-c", killScript)
		if err != nil {
			killErr = fmt.Errorf("kill checkpoint process tree: %w: %s", err, killOut)
		}
	} else {
		killOut, err := commandOutput(ctx, "docker", "kill", container)
		if err != nil {
			killErr = fmt.Errorf("kill checkpoint container: %w: %s", err, killOut)
		}
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		if killErr == nil {
			killErr = errors.New("checkpoint process did not exit within 20s of SIGKILL")
		}
	}
	if err := os.WriteFile(eventsPath, output.Bytes(), 0o644); err != nil && killErr == nil {
		killErr = err
	}
	return true, killErr
}

func (r *runner) probe(ctx context.Context, container, hostHome, containerHome, sessionRef string, resume bool, eventsPath string) ProbeResult {
	mode := "snapshot_cold_start"
	if resume {
		mode = "resume_plus_snapshot"
	}
	result := ProbeResult{Mode: mode, Events: filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(eventsPath)), filepath.Base(eventsPath)))}
	started := time.Now()
	commandCtx, cancel := context.WithTimeout(ctx, r.config.CommandTimeout)
	defer cancel()

	outputName := strings.TrimSuffix(filepath.Base(eventsPath), ".events.jsonl") + ".output.json"
	containerOutput := filepath.Join(containerHome, outputName)
	hostOutput := filepath.Join(hostHome, outputName)
	_ = os.Remove(hostOutput)
	common := []string{
		"--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "--ignore-rules", "--output-schema", "/workspace/probe-schema.json",
		"-o", containerOutput,
	}
	var codexArgs []string
	if resume {
		codexArgs = append([]string{"codex", "exec", "resume"}, common...)
		codexArgs = append(codexArgs, sessionRef, probePrompt)
	} else {
		codexArgs = append([]string{"codex", "exec"}, common...)
		codexArgs = append(codexArgs, probePrompt)
	}
	args := dockerExecArgs(container, containerHome, codexArgs...)
	out, err := exec.CommandContext(commandCtx, "docker", args...).CombinedOutput()
	result.Duration = time.Since(started)
	if writeErr := os.WriteFile(eventsPath, out, 0o644); writeErr != nil {
		result.CLIError = writeErr.Error()
	}
	_, result.Usage = parseCodexEvents(out)
	if err != nil {
		result.CLIError = fmt.Sprintf("%v: %s", err, tail(out, 1200))
	}
	answerBytes, readErr := os.ReadFile(hostOutput)
	if readErr != nil {
		result.ParseError = fmt.Sprintf("read final answer: %v", readErr)
		return result
	}
	answer, parseErr := parseProbeAnswer(answerBytes)
	if parseErr != nil {
		result.ParseError = parseErr.Error()
		return result
	}
	result.Answer = answer
	result.Score = scoreAnswer(answer)
	return result
}

func dockerExecArgs(container, home string, command ...string) []string {
	args := []string{"exec", "--workdir", "/workspace", "--env", "CODEX_HOME=" + home, "--env", "HOME=" + home, container}
	return append(args, command...)
}

func (r *runner) containerName(scenario, phase string) string {
	return "conveyor-resume-" + strings.ToLower(r.runID) + "-" + strings.ReplaceAll(scenario, "_", "-") + "-" + phase
}

func removeContainer(name string) error {
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := commandOutput(ctx, "docker", "rm", "--force", name)
	return err
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr := input.Close()
		closeErr := output.Close()
		return errors.Join(copyErr, inputCloseErr, closeErr)
	})
}

func tail(data []byte, maximum int) string {
	if len(data) <= maximum {
		return strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(string(data[len(data)-maximum:]))
}

func parseProbeAnswer(data []byte) (ProbeAnswer, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("```")) {
		lines := bytes.Split(trimmed, []byte("\n"))
		if len(lines) >= 3 {
			trimmed = bytes.Join(lines[1:len(lines)-1], []byte("\n"))
		}
	}
	var answer ProbeAnswer
	if err := json.Unmarshal(trimmed, &answer); err != nil {
		return ProbeAnswer{}, fmt.Errorf("parse final answer JSON: %w: %s", err, tail(trimmed, 600))
	}
	return answer, nil
}

func parseCodexEvents(data []byte) (string, TokenUsage) {
	var sessionRef string
	var usage TokenUsage
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Usage    struct {
				InputTokens           int64 `json:"input_tokens"`
				CachedInputTokens     int64 `json:"cached_input_tokens"`
				OutputTokens          int64 `json:"output_tokens"`
				ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type == "thread.started" && event.ThreadID != "" {
			sessionRef = event.ThreadID
		}
		if event.Type == "turn.completed" {
			usage.InputTokens += event.Usage.InputTokens
			usage.CachedInputTokens += event.Usage.CachedInputTokens
			usage.OutputTokens += event.Usage.OutputTokens
			usage.ReasoningOutputTokens += event.Usage.ReasoningOutputTokens
		}
	}
	uncached := usage.InputTokens - usage.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	usage.EffectiveTokens = uncached + usage.OutputTokens
	return sessionRef, usage
}
