package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/jobartifact"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeRunner struct {
	startErr  error
	streamErr error
	specs     []runner.StartJobSpec
	eventLog  string
}

type noCapacityPool struct{}

func (noCapacityPool) RescueTaskCredentialLeases(context.Context, string, string) error { return nil }

func (noCapacityPool) ClaimCredential(context.Context, routing.ClaimRequest) (routing.Credential, error) {
	return routing.Credential{}, routing.ErrNoCapacity
}
func (noCapacityPool) ReleaseCredential(context.Context, string, string, string) error { return nil }
func (noCapacityPool) ThrottleCredential(context.Context, string, string, string, int64) error {
	return nil
}

func TestRunTaskLeavesTaskQueuedUntilCapacityIsClaimed(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	d.Router = routing.New(noCapacityPool{}, config.Routing{OwnerID: "operator"})

	err := d.runTask(context.Background(), task.ID)
	if !errors.Is(err, routing.ErrNoCapacity) {
		t.Fatalf("runTask error = %v, want no capacity", err)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskQueued {
		t.Fatalf("task state = %s, want queued", persisted.State)
	}
	if len(f.specs) != 0 {
		t.Fatalf("runner started without capacity: %+v", f.specs)
	}
}

func TestRunTaskPropagatesSecretsPolicyAndDeterministicMount(t *testing.T) {
	f := &fakeRunner{}
	d, _, task := testDispatcher(t, f)
	d.Cfg.Repos[0].SecretRefs = []string{"secretref://test/integration/CANARY"}
	d.Cfg.Repos[0].ToolPolicy = adapter.ToolPolicy{
		AllowedCommands: [][]string{{"git"}},
		DeniedCommands:  [][]string{{"printenv"}},
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("specs = %d", len(f.specs))
	}
	spec := f.specs[0]
	if len(spec.SecretRefs) != 1 || spec.SecretRefs[0] != "secretref://test/integration/CANARY" {
		t.Fatalf("secret refs = %v", spec.SecretRefs)
	}
	if len(spec.Policy.DeniedCommands) != 1 || spec.Policy.DeniedCommands[0][0] != "printenv" {
		t.Fatalf("policy = %+v", spec.Policy)
	}
	wantPath := gitx.SandboxPath(task.ID, "api")
	if spec.Workdir != wantPath || len(spec.Worktrees) != 1 || spec.Worktrees[0].SandboxPath != wantPath {
		t.Fatalf("workdir/mount = %q %+v, want %q", spec.Workdir, spec.Worktrees, wantPath)
	}
	if strings.Contains(spec.Worktrees[0].HostPath, d.Cfg.CacheDir) {
		t.Fatalf("bare cache was mounted: %+v", spec.Worktrees)
	}
	if spec.ControlPath != "/conveyor/control" {
		t.Fatalf("control path = %q", spec.ControlPath)
	}
}

func (f *fakeRunner) Name() string { return "fake" }

func (f *fakeRunner) StartJob(_ context.Context, spec runner.StartJobSpec) (runner.JobHandle, error) {
	f.specs = append(f.specs, spec)
	if f.startErr != nil {
		return "", f.startErr
	}
	var err error
	f.eventLog, err = jobartifact.EventLogPath(spec.ControlDir, spec.JobID)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(f.eventLog, []byte("{\"kind\":\"done\",\"phase\":\"job\",\"at\":\"2026-01-01T00:00:00Z\"}\n"), 0o600); err != nil {
		return "", err
	}
	return runner.JobHandle(spec.JobID), nil
}

func (f *fakeRunner) StreamLogs(_ context.Context, _ runner.JobHandle) (<-chan runner.LogEvent, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan runner.LogEvent)
	close(ch)
	return ch, nil
}

func (f *fakeRunner) Signal(context.Context, runner.JobHandle, runner.Signal) error { return nil }

func (f *fakeRunner) CollectArtifacts(context.Context, runner.JobHandle) (runner.Artifacts, error) {
	return runner.Artifacts{ExitCode: 0, EventLog: f.eventLog}, nil
}

func TestRunTaskPersistsBootDiagnostics(t *testing.T) {
	f := &fakeRunner{startErr: &runner.BootError{
		Diagnostics: runner.BootDiagnostics{RuntimeError: "docker unavailable"},
		Err:         errors.New("exit status 1"),
	}}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err == nil {
		t.Fatal("expected boot failure")
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobSandboxBootFail {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].BootDiagnostics == nil || jobs[0].BootDiagnostics.RuntimeError != "docker unavailable" {
		t.Fatalf("boot diagnostics = %+v", jobs[0].BootDiagnostics)
	}
	if jobs[0].EndedAt.IsZero() {
		t.Fatal("boot-failed job has no end time")
	}
}

func TestRunTaskKeepsSuccessWhenLiveLogStreamFails(t *testing.T) {
	f := &fakeRunner{streamErr: errors.New("logs unavailable")}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobDone {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].EndedAt.IsZero() {
		t.Fatal("completed job has no end time")
	}
	transcript, err := st.GetTranscript(context.Background(), jobs[0].ID)
	if err != nil || transcript.URI == "" {
		t.Fatalf("transcript = %+v, err=%v", transcript, err)
	}
}

func TestInspectTranscriptAggregatesSafeMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := strings.Join([]string{
		`{"kind":"assistant_text","phase":"main","text":"Implemented the parser fix.","at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"token_usage","phase":"main","usage":{"in":11,"out":3},"cost_usd":0.25,"at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"redaction","payload":{"exact":2,"encoded":1,"pattern":3,"entropy":4},"at":"2026-01-01T00:00:01Z"}`,
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:02Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := inspectTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.tokensIn != 11 || summary.tokensOut != 3 || summary.costUSD != 0.25 || summary.agentSummary != "Implemented the parser fix." || summary.redactions != (core.RedactionStats{Exact: 2, Encoded: 1, Pattern: 3, Entropy: 4}) {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestGitHubSource(t *testing.T) {
	slug, number, ok := githubSource("github:acme/api#42")
	if !ok || slug != "acme/api" || number != 42 {
		t.Fatalf("source = %q #%d ok=%v", slug, number, ok)
	}
	for _, invalid := range []string{"cli", "github:acme/api", "github:#0", "github:acme/api#nope"} {
		if _, _, ok := githubSource(invalid); ok {
			t.Fatalf("accepted invalid source %q", invalid)
		}
	}
}

func TestRunTaskUsesUniqueAttemptIDs(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 2 {
		t.Fatalf("start specs = %d, want 2", len(f.specs))
	}
	if f.specs[0].JobID != task.ID+"-implement-1" || f.specs[1].JobID != task.ID+"-implement-2" {
		t.Fatalf("job IDs = %q, %q", f.specs[0].JobID, f.specs[1].JobID)
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
}

func TestRunTaskBriefsSuccessorWithHandoff(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)

	control := filepath.Join(d.Cfg.JobsDir, "task-"+task.ID, ".conveyor")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	prior := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement,
		State: core.JobDone, StartedAt: time.Now().Add(-time.Minute)}
	if err := st.CreateJob(context.Background(), prior); err != nil {
		t.Fatal(err)
	}
	h := &snapshot.Handoff{TaskID: task.ID, JobID: prior.ID, State: "parser half-rewritten",
		Todos: []string{"wire the fallback"}}
	handoffPath, err := snapshot.Path(control, prior.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Save(handoffPath); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(context.Background(), core.Intervention{
		TaskID: task.ID, JobID: prior.ID, Action: core.InterventionRedirect,
		ReasonCode: "spec-wrong", Comment: "Preserve the public parser API.",
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(control, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"parser half-rewritten", "wire the fallback", "handoff document", "Preserve the public parser API."} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestInspectTranscriptUsesPhaseAwareCostAndStructuredTerminalRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := strings.Join([]string{
		`{"kind":"assistant_text","phase":"main","text":"Document rate limit behavior.","cost_usd":0.25,"at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"error","phase":"handoff_resume","err":"status 429 during optional handoff","cost_usd":0.30,"at":"2026-01-01T00:00:01Z"}`,
		`{"kind":"assistant_text","phase":"handoff_fallback","cost_usd":0.10,"at":"2026-01-01T00:00:02Z"}`,
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:03Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := inspectTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.rateLimited {
		t.Fatal("successful transcript was classified as rate limited")
	}
	if diff := summary.costUSD - 0.40; diff < -0.000001 || diff > 0.000001 {
		t.Fatalf("cost = %f, want 0.40", summary.costUSD)
	}

	failedPath := filepath.Join(t.TempDir(), "failed.jsonl")
	if err := os.WriteFile(failedPath, []byte(`{"kind":"error","phase":"job","err":"status 429: rate limit exceeded","at":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed, err := inspectTranscript(failedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !failed.rateLimited {
		t.Fatal("terminal structured rate-limit error was not classified")
	}
}

func TestRunTaskPersistsAttemptScopedTranscriptURIs(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	first, err := st.GetTranscript(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.GetTranscript(context.Background(), jobs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.URI == second.URI || !strings.Contains(first.URI, jobs[0].ID) || !strings.Contains(second.URI, jobs[1].ID) {
		t.Fatalf("transcript URIs are not attempt scoped: %q %q", first.URI, second.URI)
	}
}

func TestRunTaskDoesNotReuseOlderHandoffWhenImmediatePredecessorHasNone(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	control := filepath.Join(d.Cfg.JobsDir, "task-"+task.ID, ".conveyor")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}

	first := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement,
		State: core.JobDone, StartedAt: time.Now().Add(-2 * time.Minute)}
	second := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement,
		State: core.JobFailed, StartedAt: time.Now().Add(-time.Minute)}
	if err := st.CreateJob(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	stale := &snapshot.Handoff{TaskID: task.ID, JobID: first.ID, State: "obsolete state", Todos: []string{"obsolete todo"}}
	stalePath, err := snapshot.Path(control, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Save(stalePath); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(context.Background(), core.Intervention{
		TaskID: task.ID, JobID: second.ID, Action: core.InterventionRedirect,
		ReasonCode: "scope-creep", Comment: "Remove the unrelated refactor.",
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(control, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "obsolete state") || strings.Contains(string(prompt), "obsolete todo") {
		t.Fatalf("prompt reused stale handoff:\n%s", prompt)
	}
	if !strings.Contains(string(prompt), "Remove the unrelated refactor.") {
		t.Fatalf("prompt omitted redirect without a handoff:\n%s", prompt)
	}
}

func testDispatcher(t *testing.T, r runner.Runner) (*Dispatcher, store.Store, core.Task) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	cfg := &config.Config{
		Workspace: "test",
		Image:     "conveyor:test",
		CacheDir:  filepath.Join(tmp, "cache"),
		JobsDir:   filepath.Join(tmp, "jobs"),
		Repos: []config.Repo{{
			Name: "api",
			URL:  "file://" + origin,
			Base: "main",
		}},
	}
	st := store.NewMemory()
	task := core.Task{
		ID:         "test-task",
		Workspace:  "test",
		Source:     "test",
		Title:      "test task",
		Repo:       "api",
		BaseBranch: "main",
		Branch:     gitx.BranchName("test-task"),
		State:      core.TaskQueued,
		CreatedAt:  time.Now(),
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	d := New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), r, cfg)
	d.Router = routing.NewStatic(routing.Credential{
		ID: "test-codex", OwnerID: "operator", OwnerKind: "user",
		Kind: "personal_sub", Vendor: "openai", Harness: "codex",
	}, cfg.Routing)
	return d, st, task
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
