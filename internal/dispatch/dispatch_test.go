package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/jobartifact"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type fakeRunner struct {
	startErr      error
	streamErr     error
	waitForCancel bool
	specs         []runner.StartJobSpec
	eventLog      string
	eventData     string
	exitCode      int
}

type deadlineInspectingRunner struct {
	fakeRunner
	collectCalls    int
	collectDeadline time.Time
	collectBounded  bool
}

func (r *deadlineInspectingRunner) CollectArtifacts(ctx context.Context, handle runner.JobHandle) (runner.Artifacts, error) {
	r.collectCalls++
	if r.collectCalls == 1 {
		r.collectDeadline, r.collectBounded = ctx.Deadline()
		return runner.Artifacts{}, errors.New("docker wait remained hung")
	}
	return r.fakeRunner.CollectArtifacts(ctx, handle)
}

type phase3Runner struct {
	outputs map[core.Stage][]string
	events  map[runner.JobHandle]string
}

func (r *phase3Runner) Name() string { return "phase3-fake" }
func (r *phase3Runner) StartJob(_ context.Context, spec runner.StartJobSpec) (runner.JobHandle, error) {
	parts := strings.Split(spec.JobID, "-")
	stage := core.Stage(parts[len(parts)-2])
	outputs := r.outputs[stage]
	if len(outputs) == 0 {
		return "", errors.New("missing scripted output for " + string(stage))
	}
	output := outputs[0]
	r.outputs[stage] = outputs[1:]
	if stage == core.StageImplement {
		worktree := spec.Worktrees[0].HostPath
		path := filepath.Join(worktree, "phase3.txt")
		file, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = file.WriteString(spec.JobID + "\n")
		_ = file.Close()
		mustRunNoTest(worktree, "git", "add", "phase3.txt")
		mustRunNoTest(worktree, "git", "commit", "-m", "test: phase 3 implementation")
	}
	path, err := jobartifact.EventLogPath(spec.ControlDir, spec.JobID)
	if err != nil {
		return "", err
	}
	data := `{"kind":"assistant_text","phase":"main","text":` + strconv.Quote(output) + `,"at":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return "", err
	}
	handle := runner.JobHandle(spec.JobID)
	if r.events == nil {
		r.events = map[runner.JobHandle]string{}
	}
	r.events[handle] = path
	return handle, nil
}
func (r *phase3Runner) StreamLogs(context.Context, runner.JobHandle) (<-chan runner.LogEvent, error) {
	ch := make(chan runner.LogEvent)
	close(ch)
	return ch, nil
}
func (r *phase3Runner) Signal(context.Context, runner.JobHandle, runner.Signal) error { return nil }
func (r *phase3Runner) CollectArtifacts(_ context.Context, handle runner.JobHandle) (runner.Artifacts, error) {
	return runner.Artifacts{ExitCode: 0, EventLog: r.events[handle]}, nil
}

func mustRunNoTest(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(string(out))
	}
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

func TestPhase3PipelineSpecGateAndBoundedReviewBounce(t *testing.T) {
	specOutput := "# Change\n\n## Intent\nImplement it.\n\n## Non-goals\nUnrelated refactors.\n\n```conveyor:acceptance\n- id: AC-1\n  criterion: Phase 3 file exists\n  verify: test\n  ref: phase3.txt\n```"
	r := &phase3Runner{outputs: map[core.Stage][]string{
		core.StageTriage:    {"```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.9,\"route\":\"spec\",\"summary\":\"Contract needed.\"}\n```"},
		core.StageSpec:      {specOutput},
		core.StageImplement: {"Implemented v1.", "Implemented review feedback."},
		core.StageReview: {
			"```conveyor:review\n{\"verdict\":\"changes_requested\",\"reason_code\":\"scope-creep\",\"summary\":\"Needs correction.\",\"feedback\":\"Tighten the implementation.\"}\n```",
			"```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"Matches the spec.\",\"feedback\":\"\"}\n```",
		},
	}}
	d, st, task := testDispatcher(t, r)
	task.Level = core.L2
	// Replace the fixture with an L2 task while preserving its repository.
	memory := store.NewMemory()
	if err := memory.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	d.Store = memory
	st = memory
	d.Cfg.PackDir = filepath.Join("..", "..", "pack")
	d.Cfg.MaxBounces = 2
	bundle, loadErr := pack.Load(d.Cfg.PackDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	d.Pack = bundle

	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(context.Background(), task.ID)
	if current.State != core.TaskAwaiting {
		t.Fatalf("after spec state=%s", current.State)
	}
	storedSpec, ok, err := st.GetLatestSpecVersion(context.Background(), task.ID)
	if err != nil || !ok || !strings.Contains(string(storedSpec.Acceptance), "AC-1") || string(storedSpec.Decomposition) != "[]" {
		t.Fatalf("stored spec=%+v ok=%v err=%v", storedSpec, ok, err)
	}
	latest, _, _ := st.GetLatestJob(context.Background(), task.ID)
	intervention := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "approved"}
	if err := st.CreateIntervention(context.Background(), intervention); err != nil {
		t.Fatal(err)
	}
	if err := d.HandleIntervention(context.Background(), task, latest, intervention); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := d.runTask(context.Background(), task.ID); err != nil {
			t.Fatal(err)
		}
	}
	current, _ = st.GetTask(context.Background(), task.ID)
	if current.State != core.TaskAwaiting {
		t.Fatalf("final state=%s", current.State)
	}
	jobs, _ := st.ListJobs(context.Background(), task.ID)
	want := []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageReview, core.StageImplement, core.StageReview}
	if len(jobs) != len(want) {
		t.Fatalf("jobs=%d want=%d", len(jobs), len(want))
	}
	for i := range want {
		if jobs[i].Stage != want[i] {
			t.Fatalf("job %d stage=%s want=%s", i, jobs[i].Stage, want[i])
		}
	}
}

func TestNextStageUsesPersistedRecoveryDecision(t *testing.T) {
	_, _, task := testDispatcher(t, &fakeRunner{})
	task.Level = core.L2
	task.State = core.TaskQueued
	task.NextStage = core.StageReview
	stage, proceed := nextStage(task)
	if !proceed || stage != core.StageReview {
		t.Fatalf("stage=%s proceed=%v", stage, proceed)
	}
}

func TestInvalidOutputAtBounceLimitPersistsHaltedRecoveryStage(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	task.Level = core.L2
	d.Cfg.MaxBounces = 1
	job := core.Job{ID: task.ID + "-triage-1", TaskID: task.ID, Stage: core.StageTriage, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := d.completeStage(context.Background(), task, d.Cfg.Repos[0], "", job, "malformed"); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskAwaiting || persisted.NextStage != "" || persisted.RecoveryStage != core.StageTriage {
		t.Fatalf("halted transition = state:%s next:%s recovery:%s", persisted.State, persisted.NextStage, persisted.RecoveryStage)
	}
}

func TestReviewBounceCapDoesNotBecomeRunnableUntilExplicitRedirect(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	task.Level = core.L2
	d.Cfg.MaxBounces = 1
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	output := "```conveyor:review\n{\"verdict\":\"changes_requested\",\"reason_code\":\"other\",\"summary\":\"fix\",\"feedback\":\"correct it\"}\n```"
	if err := d.completeStage(context.Background(), task, d.Cfg.Repos[0], "", job, output); err != nil {
		t.Fatal(err)
	}
	halted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if halted.State != core.TaskAwaiting || halted.NextStage != "" || halted.RecoveryStage != core.StageImplement {
		t.Fatalf("halted transition = %+v", halted)
	}
	intervention := core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionRedirect, ReasonCode: "other", Comment: "retry"}
	if err := d.HandleIntervention(context.Background(), halted, job, intervention); err != nil {
		t.Fatal(err)
	}
	resumed, _ := st.GetTask(context.Background(), task.ID)
	if resumed.State != core.TaskQueued || resumed.NextStage != core.StageImplement || resumed.RecoveryStage != "" {
		t.Fatalf("resumed transition = %+v", resumed)
	}
}

func TestApprovePausedJobResumesSamePersistedStage(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	task.Level = core.L2
	task.State = core.TaskAwaiting
	task.RecoveryStage = core.StageSpec
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskAwaiting, "", core.StageSpec); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPaused, StartedAt: time.Now()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := d.HandleIntervention(context.Background(), task, job, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	resumed, _ := st.GetTask(context.Background(), task.ID)
	if resumed.State != core.TaskQueued || resumed.NextStage != core.StageSpec {
		t.Fatalf("resumed transition = %+v", resumed)
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
	eventData := f.eventData
	if eventData == "" {
		eventData = "{\"kind\":\"done\",\"phase\":\"job\",\"at\":\"2026-01-01T00:00:00Z\"}\n"
	}
	if err := os.WriteFile(f.eventLog, []byte(eventData), 0o600); err != nil {
		return "", err
	}
	return runner.JobHandle(spec.JobID), nil
}

func (f *fakeRunner) StreamLogs(ctx context.Context, _ runner.JobHandle) (<-chan runner.LogEvent, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan runner.LogEvent)
	if f.waitForCancel {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	close(ch)
	return ch, nil
}

func (f *fakeRunner) Signal(context.Context, runner.JobHandle, runner.Signal) error { return nil }

func (f *fakeRunner) CollectArtifacts(context.Context, runner.JobHandle) (runner.Artifacts, error) {
	return runner.Artifacts{ExitCode: f.exitCode, EventLog: f.eventLog}, nil
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

func TestInspectTranscriptUsesLastAssistantOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := strings.Join([]string{
		`{"kind":"assistant_text","phase":"main","text":"draft","at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"assistant_text","phase":"main","text":"final","at":"2026-01-01T00:00:01Z"}`,
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:02Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := inspectTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.agentOutput != "final" {
		t.Fatalf("agent output=%q", summary.agentOutput)
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
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskQueued, core.StageImplement, ""); err != nil {
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

func TestInspectTranscriptReturnsPartialUsageWithoutTerminalEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := strings.Join([]string{
		`{"kind":"token_usage","phase":"main","usage":{"in":120,"out":30},"cost_usd":0.60,"at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"assistant_text","phase":"main","text":"partial result","usage":{"in":20,"out":5},"cost_usd":0.75,"at":"2026-01-01T00:00:01Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := inspectTranscript(path)
	if err == nil || !strings.Contains(err.Error(), "no terminal event") {
		t.Fatalf("error = %v", err)
	}
	if summary.tokensIn != 140 || summary.tokensOut != 35 || summary.costUSD != 0.75 || summary.agentOutput != "partial result" {
		t.Fatalf("partial summary = %+v", summary)
	}
}

func TestTimedOutJobPersistsPartialTranscriptUsage(t *testing.T) {
	f := &fakeRunner{waitForCancel: true, eventData: strings.Join([]string{
		`{"kind":"token_usage","phase":"main","usage":{"in":120,"out":30},"cost_usd":0.60,"at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"assistant_text","phase":"main","text":"partial result","usage":{"in":20,"out":5},"cost_usd":0.75,"at":"2026-01-01T00:00:01Z"}`,
	}, "\n") + "\n"}
	d, st, task := testDispatcher(t, f)
	d.Cfg.Routing.Stages = map[string]config.StageRoute{
		string(core.StageImplement): {Timeout: 5 * time.Millisecond},
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobPaused || jobs[0].TokensIn != 140 || jobs[0].TokensOut != 35 || jobs[0].CostUSD != 0.75 {
		t.Fatalf("timed-out job lost partial usage: %+v", jobs)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskAwaiting || persisted.RecoveryStage != core.StageImplement {
		t.Fatalf("timeout recovery = %+v", persisted)
	}
}

func TestEarlyLogFailureStillBoundsArtifactCollection(t *testing.T) {
	r := &deadlineInspectingRunner{fakeRunner: fakeRunner{streamErr: errors.New("docker log stream disconnected")}}
	d, _, task := testDispatcher(t, r)
	d.Cfg.Routing.Stages = map[string]config.StageRoute{
		string(core.StageImplement): {Timeout: 50 * time.Millisecond},
	}
	started := time.Now()
	err := d.runTask(context.Background(), task.ID)
	if err == nil || !strings.Contains(err.Error(), "collect artifacts") {
		t.Fatalf("runTask error = %v", err)
	}
	if !r.collectBounded {
		t.Fatal("artifact collection received an unbounded context")
	}
	remaining := r.collectDeadline.Sub(started)
	want := 50*time.Millisecond + artifactCollectionMargin
	if remaining < want-time.Second || remaining > want+time.Second {
		t.Fatalf("artifact deadline remaining = %s, want about %s", remaining, want)
	}
	if r.collectCalls != 2 {
		t.Fatalf("collect calls = %d, want failed collection plus bounded deferred cleanup", r.collectCalls)
	}
}

func TestSuccessfulJobAtCumulativeBudgetKeepsCompletedOutput(t *testing.T) {
	f := &fakeRunner{eventData: strings.Join([]string{
		`{"kind":"assistant_text","phase":"main","text":"implementation complete","cost_usd":2.50,"at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"token_usage","phase":"handoff_resume","usage":{"in":10,"out":5},"cost_usd":3.00,"at":"2026-01-01T00:00:01Z"}`,
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:02Z"}`,
	}, "\n") + "\n"}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobDone || jobs[0].CostUSD != 3.0 {
		t.Fatalf("jobs = %+v", jobs)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskParked {
		t.Fatalf("task state = %s, want completed output processed into parked/no-commit result", persisted.State)
	}
}

func TestReviewStageDoesNotReceivePriorRedirectFeedback(t *testing.T) {
	f := &fakeRunner{eventData: strings.Join([]string{
		`{"kind":"assistant_text","phase":"main","text":"\u0060\u0060\u0060conveyor:review\\n{\\\"verdict\\\":\\\"approve\\\",\\\"reason_code\\\":\\\"approved\\\",\\\"summary\\\":\\\"clean\\\",\\\"feedback\\\":\\\"\\\"}\\n\u0060\u0060\u0060","at":"2026-01-01T00:00:00Z"}`,
		`{"kind":"done","phase":"job","at":"2026-01-01T00:00:01Z"}`,
	}, "\n") + "\n"}
	d, st, task := testDispatcher(t, f)
	task.Level = core.L2
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskQueued, core.StageReview, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(context.Background(), core.Intervention{
		TaskID: task.ID, Action: core.InterventionRedirect, ReasonCode: "scope-creep", Comment: "Remove the unrelated refactor.",
	}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load(filepath.Join("..", "..", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	d.Pack = bundle
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(d.Cfg.JobsDir, "task-"+task.ID, ".conveyor", "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "Remove the unrelated refactor.") || strings.Contains(string(prompt), "Human reviewer feedback") {
		t.Fatalf("review prompt was primed by prior redirect:\n%s", prompt)
	}
}

func TestPollReviewFeedbackRequiresConveyorOpenedPR(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	d.Cfg.Repos[0].GitHub = "acme/api"
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskAwaiting, "", core.StageImplement); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d.reviewFeedback = func(context.Context, string, string, github.ReviewCursor) (github.ReviewFeedbackPage, error) {
		calls++
		return github.ReviewFeedbackPage{PR: 8, State: "open"}, nil
	}
	d.pollReviewFeedback(context.Background())
	if calls != 0 {
		t.Fatalf("feedback API calls = %d, want none before pull_request.opened", calls)
	}
}

func TestPollReviewFeedbackUsesCursorAndSharesBounceBudget(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	d.Cfg.Repos[0].GitHub = "acme/api"
	d.Cfg.MaxBounces = 1
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskAwaiting, "", core.StageImplement); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(context.Background(), core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pull_request.opened"}); err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 7, 12, 7, 0, 0, 0, time.UTC)
	if err := st.AppendEvent(context.Background(), core.Event{TaskID: task.ID, Kind: "github.review_polled", Payload: core.JSONPayload(map[string]any{"cursor": wantSince})}); err != nil {
		t.Fatal(err)
	}
	var gotCursor github.ReviewCursor
	d.reviewFeedback = func(_ context.Context, _, _ string, cursor github.ReviewCursor) (github.ReviewFeedbackPage, error) {
		gotCursor = cursor
		return github.ReviewFeedbackPage{PR: 8, State: "open", Cursor: github.ReviewCursor{ReviewAfter: "cursor-2"}, Feedback: []github.ReviewFeedback{
			{ID: "review:R1", Author: "alice", Body: "Fix the retry.", PR: 8},
			{ID: "comment:91", Author: "bob", Body: "Cover the timeout.", PR: 8},
		}}, nil
	}
	d.pollReviewFeedback(context.Background())
	if !gotCursor.Since.Equal(wantSince) {
		t.Fatalf("since = %s, want %s", gotCursor.Since, wantSince)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskAwaiting || persisted.NextStage != "" || persisted.RecoveryStage != core.StageImplement {
		t.Fatalf("task bypassed bounce cap: %+v", persisted)
	}
	bounces, err := st.CountEvents(context.Background(), task.ID, "pipeline.bounced")
	if err != nil || bounces != 1 {
		t.Fatalf("bounces = %d, err=%v", bounces, err)
	}
	interventions, err := st.ListInterventions(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(interventions) != 1 || !strings.Contains(interventions[0].Comment, "alice: Fix the retry.") || !strings.Contains(interventions[0].Comment, "bob: Cover the timeout.") {
		t.Fatalf("batched intervention = %+v", interventions)
	}
	events, err := st.ListEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, persistedCursor, _ := reviewPollState(events)
	if persistedCursor.ReviewAfter != "cursor-2" || !persistedCursor.Since.After(wantSince) {
		t.Fatalf("persisted review cursor = %+v", persistedCursor)
	}
}

func TestPollReviewFeedbackNeverRequeuesMergedPR(t *testing.T) {
	d, st, task := testDispatcher(t, &fakeRunner{})
	d.Cfg.Repos[0].GitHub = "acme/api"
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskApproved, "", ""); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(context.Background(), core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pull_request.opened"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d.reviewFeedback = func(context.Context, string, string, github.ReviewCursor) (github.ReviewFeedbackPage, error) {
		calls++
		return github.ReviewFeedbackPage{PR: 8, State: "merged", Feedback: []github.ReviewFeedback{{ID: "comment:91", Author: "alice", Body: "late comment", PR: 8}}}, nil
	}
	d.pollReviewFeedback(context.Background())
	d.pollReviewFeedback(context.Background())
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || persisted.State != core.TaskApproved {
		t.Fatalf("calls=%d task=%+v", calls, persisted)
	}
	interventions, err := st.ListInterventions(context.Background(), task.ID)
	if err != nil || len(interventions) != 0 {
		t.Fatalf("merged PR interventions = %+v, err=%v", interventions, err)
	}
}

func TestRunTaskPersistsAttemptScopedTranscriptURIs(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskTransition(context.Background(), task.ID, core.TaskQueued, core.StageImplement, ""); err != nil {
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
